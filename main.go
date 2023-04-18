package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/robfig/cron/v3"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
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

type objectEvent struct {
	obj metav1.Object
	e   watch.Event
}

type cronjob struct {
	entryID  cron.EntryID
	schedule string
}

var resourcesMap = map[string]string{}

func main() {
	klog.InitFlags(flag.CommandLine)
	flag.Parse()

	appsV1Client, err := makeAppsV1Client()
	if err != nil {
		klog.Fatalf("makeAppsV1Client error: %s", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	eventCh := make(chan objectEvent, 20)

	if *enableDeployment {
		go watchResource(ctx, appsV1Client, "deployments", &appsv1.Deployment{}, eventCh)
	}

	if *enableDaemonset {
		go watchResource(ctx, appsV1Client, "daemonsets", &appsv1.DaemonSet{}, eventCh)
	}

	if *enableStatefulset {
		go watchResource(ctx, appsV1Client, "statefulsets", &appsv1.StatefulSet{}, eventCh)
	}

	c := cron.New()
	go handleEvent(eventCh, c, appsV1Client)
	c.Start()

	<-ctx.Done()
	close(eventCh)

	<-c.Stop().Done()
}

func handleEvent(ch chan objectEvent, c *cron.Cron, rc rest.Interface) {
	cronjobs := make(map[string]cronjob)

	validNamespace := (func() func(ns string) bool {
		var watchNs []string

		if *watchNamespaces == "" {
			klog.Info("watch all namespaces")
		} else {
			watchNs = strings.Split(*watchNamespaces, ",")
		}

		return func(ns string) bool {
			if len(watchNs) == 0 {
				return true
			}

			for _, _ns := range watchNs {
				if ns == _ns {
					return true
				}
			}

			return false
		}
	})()

	for event := range ch {
		ns := event.obj.GetNamespace()

		if !validNamespace(ns) {
			continue
		}

		name := event.obj.GetName()
		objType := fmt.Sprintf("%T", event.obj)

		cronkey := fmt.Sprintf("%s/%s/%s", objType, ns, name)
		schedule := event.obj.GetAnnotations()[*cronScheduleAnnotation]

		var (
			needDelete, needAdd bool
		)

		switch event.e.Type {
		case watch.Added:
			if schedule != "" {
				needAdd = true
			}
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
				cronjobs[cronkey] = cronjob{
					entryID:  entryID,
					schedule: schedule,
				}

				klog.InfoS("add cronjob", "kind", objType, "ns", ns, "name", name, "schedule", schedule)
			}
		}
	}
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

func watchResource[T metav1.Object](ctx context.Context, client rest.Interface, resource string, sampleObj T, ch chan objectEvent) {
	resourcesMap[fmt.Sprintf("%T", sampleObj)] = resource

	var (
		watcher watch.Interface
		err     error
		done    bool
	)

	opts := &metav1.ListOptions{Watch: true}

	go func() {
		<-ctx.Done()
		done = true

		if watcher != nil {
			watcher.Stop()
		}
	}()

	for {
		watcher, err = client.Get().
			Namespace(corev1.NamespaceAll).
			Resource(resource).
			VersionedParams(opts, scheme.ParameterCodec).
			Watch(ctx)

		if err != nil {
			klog.Errorf("watch %s error: %s", resource, err)
			time.Sleep(time.Second * 5)
			continue
		}

		for event := range watcher.ResultChan() {
			if event.Type == watch.Error {
				if v, ok := event.Object.(*metav1.Status); ok {
					klog.Errorf("watch %s error: %s: %s", resource, v.Reason, v.Message)
				} else {
					klog.Errorf("watch %s error", resource)
				}
				continue
			}

			if obj, ok := event.Object.(T); ok {
				ch <- objectEvent{
					obj: obj,
					e:   event,
				}
			} else {
				klog.Errorf("cast event object to %T failed", *(new(T)))
			}
		}

		if done {
			return
		}
	}
}

func makeAppsV1Client() (rest.Interface, error) {
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

	gv := appsv1.SchemeGroupVersion
	config.GroupVersion = &gv
	config.APIPath = "/apis"
	config.NegotiatedSerializer = scheme.Codecs.WithoutConversion()

	if config.UserAgent == "" {
		config.UserAgent = rest.DefaultKubernetesUserAgent()
	}

	return rest.RESTClientFor(config)
}
