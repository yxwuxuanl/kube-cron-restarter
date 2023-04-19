package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/robfig/cron/v3"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const restartedAnnotation = "cron-restarter/restartedAt"

var (
	enableDeployment       = flag.Bool("deployment", true, "")
	enableDaemonset        = flag.Bool("daemonset", false, "")
	enableStatefulset      = flag.Bool("statefulset", false, "")
	cronScheduleAnnotation = flag.String("schedule-annotation", "cron-restarter/schedule", "")
	watchNamespaces        = flag.String("namespaces", "", "")
)

type Object interface {
	runtime.Object
	metav1.Object
}

type Event struct {
	obj   Object
	event watch.EventType
}

type Cronjob struct {
	entryID  cron.EntryID
	schedule string
}

var resourcesMap = map[string]string{}

var validNamespace func(ns string) bool

func main() {
	klog.InitFlags(flag.CommandLine)

	kc, err := makeKubeClient()
	if err != nil {
		klog.Fatalf("make kube client error: %s", err)
	}

	flag.Parse()

	var watchNs []string
	if *watchNamespaces != "" {
		watchNs = strings.Split(*watchNamespaces, ",")
	}

	validNamespace = func(ns string) bool {
		if len(watchNs) == 0 {
			return true
		}

		for _, _ns := range watchNs {
			if _ns == ns {
				return true
			}
		}

		return false
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	eventCh := make(chan Event, 20)

	appsv1Client := kc.AppsV1().RESTClient()

	if *enableDeployment {
		go watchResource(ctx, appsv1Client, "deployments", &appsv1.Deployment{}, eventCh)
	}

	if *enableDaemonset {
		go watchResource(ctx, appsv1Client, "daemonsets", &appsv1.DaemonSet{}, eventCh)
	}

	if *enableStatefulset {
		go watchResource(ctx, appsv1Client, "statefulsets", &appsv1.StatefulSet{}, eventCh)
	}

	c := cron.New()
	go handleEvent(eventCh, c, appsv1Client)
	c.Start()

	<-ctx.Done()
	close(eventCh)

	<-c.Stop().Done()
}

func watchResource[T Object](
	ctx context.Context,
	client rest.Interface,
	resource string,
	sampleObj T,
	ch chan Event,
) {
	listWatcher := cache.NewListWatchFromClient(client, resource, corev1.NamespaceAll, fields.Everything())

	_, informer := cache.NewIndexerInformer(listWatcher, sampleObj, 0, cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if o, ok := obj.(T); ok {
				if !validNamespace(o.GetNamespace()) || getSchedule(o) == "" {
					return
				}

				ch <- Event{
					obj:   o,
					event: watch.Added,
				}
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			if o, ok := newObj.(T); ok {
				if !validNamespace(o.GetNamespace()) {
					return
				}

				if getSchedule(o) == "" {
					if oo, ok := oldObj.(T); ok && getSchedule(oo) == "" {
						return
					}
				}

				ch <- Event{
					obj:   o,
					event: watch.Modified,
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			if o, ok := obj.(T); ok {
				if !validNamespace(o.GetNamespace()) || getSchedule(o) == "" {
					return
				}

				ch <- Event{
					obj:   o,
					event: watch.Deleted,
				}
			}
		},
	}, cache.Indexers{})

	resourcesMap[typeOf(sampleObj)] = resource
	informer.Run(ctx.Done())
}

func handleEvent(ch chan Event, c *cron.Cron, rc rest.Interface) {
	cronjobs := make(map[string]Cronjob)

	for event := range ch {
		ns := event.obj.GetNamespace()

		name := event.obj.GetName()
		objType := typeOf(event.obj)

		cronkey := fmt.Sprintf("%s/%s/%s", objType, ns, name)
		schedule := getSchedule(event.obj)

		var (
			needDelete, needAdd bool
		)

		switch event.event {
		case watch.Added:
			needAdd = true
		case watch.Modified:
			if rec, ok := cronjobs[cronkey]; ok {
				if rec.schedule == schedule {
					continue
				}
				needDelete = true
			}

			if schedule != "" {
				needAdd = true
			}
		case watch.Deleted:
			needDelete = true
		}

		if needDelete {
			if rec, ok := cronjobs[cronkey]; ok {
				c.Remove(rec.entryID)
				delete(cronjobs, cronkey)
				klog.InfoS("delete cronjob", "kind", objType, "ns", ns, "name", name)
			}
		}

		if needAdd {
			if rec, ok := cronjobs[cronkey]; ok {
				if rec.schedule == schedule {
					continue
				}
			}

			entryID, err := c.AddFunc(schedule, func() {
				patch := buildPodTemplateAnnotationsPatch(restartedAnnotation, time.Now().Format(time.RFC3339))
				if err := patchObject(rc, resourcesMap[objType], ns, name, patch); err != nil {
					klog.ErrorS(err, "patch error", "kind", objType, "ns", ns, "name", name)
				} else {
					klog.InfoS("patch succeeded", "kind", objType, "ns", ns, "name", name)
				}
			})

			if err != nil {
				klog.ErrorS(err, "add cronjob error", "kind", objType, "ns", ns, "name", name, "schedule", schedule)
			} else {
				cronjobs[cronkey] = Cronjob{
					entryID:  entryID,
					schedule: schedule,
				}

				klog.InfoS("add cronjob", "kind", objType, "ns", ns, "name", name, "schedule", schedule)
			}
		}
	}
}

func makeKubeClient() (kubernetes.Interface, error) {
	config, err := rest.InClusterConfig()

	if err != nil {
		kubeconfPath := os.Getenv("KUBECONFIG")
		if kubeconfPath == "" {
			kubeconfPath = os.Getenv("HOME") + "/.kube/config"
		}
		if err == rest.ErrNotInCluster {
			config, err = clientcmd.BuildConfigFromFlags("", kubeconfPath)
		}
	}

	if config == nil {
		return nil, err
	}

	return kubernetes.NewForConfig(config)
}

func typeOf(o any) string {
	return fmt.Sprintf("%T", o)
}

func patchObject(rc rest.Interface, resource, ns, name string, patch []byte) error {
	return rc.Patch(types.MergePatchType).
		Resource(resource).
		Namespace(ns).
		Name(name).
		Body(patch).
		VersionedParams(&metav1.PatchOptions{}, scheme.ParameterCodec).
		Do(context.Background()).
		Error()
}

func buildPodTemplateAnnotationsPatch(key, value string) []byte {
	patch := fmt.Sprintf(`
		{
			"spec": {
				"template": {
					"metadata": {
						"annotations": {
							"%s":"%s"
						}
					}
				}
			}
		}`, key, value)

	return []byte(patch)
}

func getSchedule(o Object) string {
	return o.GetAnnotations()[*cronScheduleAnnotation]
}
