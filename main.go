package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/robfig/cron/v3"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
	"os"
	"os/signal"
	"path"
	"syscall"
	"time"
)

var (
	enableDeployment  = flag.Bool("deployments", true, "")
	enableDaemonset   = flag.Bool("daemonsets", false, "")
	enableStatefulset = flag.Bool("statefulsets", false, "")

	cronScheduleAnnotation = flag.String("schedule-annotation", "cron-restarter/schedule", "")
)

func main() {
	flag.Parse()

	kconfig, err := rest.InClusterConfig()
	if err != nil {
		if errors.Is(err, rest.ErrNotInCluster) {
			kconfig, err = clientcmd.BuildConfigFromFlags(
				"",
				path.Join(os.Getenv("HOME"), ".kube/config"),
			)
		}
	}

	if err != nil {
		panic(err)
	}

	clientset := kubernetes.NewForConfigOrDie(kconfig)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	c := cron.New()
	c.Start()

	if *enableDeployment {
		go watch(ctx, clientset.AppsV1().RESTClient(), "deployments", &appsv1.Deployment{}, c)
	}

	if *enableStatefulset {
		go watch(ctx, clientset.AppsV1().RESTClient(), "statefulsets", &appsv1.StatefulSet{}, c)
	}

	if *enableDaemonset {
		go watch(ctx, clientset.AppsV1().RESTClient(), "daemonsets", &appsv1.DaemonSet{}, c)
	}

	<-ctx.Done()
	<-c.Stop().Done()
}

func watch(
	ctx context.Context,
	client rest.Interface,
	resource string,
	objType runtime.Object,
	c *cron.Cron,
) {
	jobs := make(map[string]cron.EntryID)

	add := func(she, key string, job func()) {
		entryID, err := c.AddFunc(she, job)
		if err != nil {
			klog.ErrorS(err, "failed to add job", "resource", resource, "key", key)
			return
		}

		jobs[key] = entryID
		klog.InfoS("job added", "resource", resource, "key", key, "schedule", she)
	}

	remove := func(key string) {
		if entryID, ok := jobs[key]; ok {
			c.Remove(entryID)
			delete(jobs, key)
			klog.InfoS("job removed", "resource", resource, "key", key)
		}
	}

	listWatch := cache.NewListWatchFromClient(client, resource, corev1.NamespaceAll, fields.Everything())
	_, controller := cache.NewIndexerInformer(listWatch, objType, 0, cache.ResourceEventHandlerFuncs{
		AddFunc: func(v interface{}) {
			obj := v.(metav1.Object)

			if she := getSchedule(obj); she != "" {
				job := buildJob(client, resource, obj.GetNamespace(), obj.GetName())
				add(she, getObjectKey(obj), job)
			}
		},
		UpdateFunc: func(oldv, newv interface{}) {
			oldObj := oldv.(metav1.Object)
			newObj := newv.(metav1.Object)

			if getSchedule(newObj) == getSchedule(oldObj) {
				return
			}

			if getSchedule(oldObj) != "" {
				remove(getObjectKey(oldObj))
			}

			if she := getSchedule(newObj); she != "" {
				job := buildJob(client, resource, newObj.GetNamespace(), newObj.GetName())
				add(she, getObjectKey(newObj), job)
			}
		},
		DeleteFunc: func(v interface{}) {
			remove(getObjectKey(v.(metav1.Object)))
		},
	}, cache.Indexers{})

	klog.Infof("watching %s", resource)
	controller.Run(ctx.Done())
}

func buildJob(
	client rest.Interface,
	resource, namespace, name string,
) func() {
	return func() {
		patch := fmt.Sprintf(`
		{
			"spec": {
				"template": {
					"metadata": {
						"annotations": {
							"cron-restarter/restartedAt":"%s"
						}
					}
				}
			}
		}`, time.Now().Format(time.RFC3339))

		err := client.Patch(types.MergePatchType).
			Resource(resource).
			Namespace(namespace).
			Name(name).
			Body([]byte(patch)).
			VersionedParams(&metav1.PatchOptions{}, scheme.ParameterCodec).
			Do(context.Background()).
			Error()

		if err != nil {
			klog.ErrorS(err, "failed to patch object", "resource", resource, "namespace", namespace, "name", name)
			return
		}

		klog.InfoS(
			"resource restarted",
			"resource", resource,
			"namespace", namespace,
			"name", name,
		)
	}
}

func getObjectKey(object metav1.Object) string {
	return object.GetNamespace() + "/" + object.GetName()
}

func getSchedule(object metav1.Object) string {
	if annos := object.GetAnnotations(); annos != nil {
		return annos[*cronScheduleAnnotation]
	}

	return ""
}
