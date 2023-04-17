package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/robfig/cron/v3"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	appstypev1 "k8s.io/client-go/kubernetes/typed/apps/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

var (
	crontabAnnotation   = "cron-restarter/cron"
	restartedAnnotation = "cron-restarter/restartedAt"
)

var (
	enableDeployment  = flag.Bool("deployment", true, "")
	enableDaemonset   = flag.Bool("daemonset", false, "")
	enableStatefulset = flag.Bool("statefulset", false, "")
	watchNamespaces   = flag.String("namespaces", "", "")
)

type watchApp interface {
	Watch(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error)
	Patch(annoKey, annoValue string) error
	GetCron(object runtime.Object) (cronkey string, crontab string)
}

type deployment struct {
	deploy appstypev1.DeploymentInterface
}

func (d deployment) GetCron(object runtime.Object) (cronkey string, crontab string) {
	deploy := object.(*appsv1.Deployment)
	cronkey = fmt.Sprintf("%s/%s/%s", deploy.Kind, deploy.Namespace, deploy.Name)
	return cronkey, deploy.Annotations[crontabAnnotation]
}

func (d deployment) Watch(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
	return d.deploy.Watch(ctx, opts)
}

func (d deployment) Patch(annoKey, annoValue string) error {
	//TODO implement me
	panic("implement me")
}

type appEvent struct {
	wa watchApp
	watch.Event
}

type cronRecord struct {
	entryID cron.EntryID
	crontab string
}

func main() {
	log.Default().SetFlags(log.LstdFlags | log.Lshortfile)

	kc, err := makeKubeClient()
	if err != nil {
		log.Fatalf("make kube client error: %s", err)
	}

	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	eventCh := make(chan appEvent, 20)

	addApp := func(wa watchApp) error {
		w, err := wa.Watch(context.Background(), metav1.ListOptions{})
		if err != nil {
			return err
		}

		go func() {
			for event := range w.ResultChan() {
				eventCh <- appEvent{
					Event: event,
					wa:    wa,
				}
			}
		}()

		<-ctx.Done()
		w.Stop()

		return nil
	}

	if *enableDaemonset {
		err := addApp(deployment{
			deploy: kc.AppsV1().Deployments(corev1.NamespaceAll),
		})

		if err != nil {
			log.Printf("watch deployment error: %s", err)
		}
	}

	c := cron.New()
	cronRecordMap := make(map[string]cronRecord)

	for event := range eventCh {
		if event.Event.Type == watch.Error {
			if status, ok := event.Object.(*metav1.Status); ok {
				log.Printf("watch %s error: %s", status.Kind, status.Message)
			} else {
				log.Printf("watch error: unknown error")
			}

			continue
		}

		cronkey, crontab := event.wa.GetCron(event.Object)

		var (
			needDelete, needAdd bool
		)

		switch event.Event.Type {
		case watch.Added:
			needAdd = true
		case watch.Modified:
			if rec, ok := cronRecordMap[cronkey]; ok {
				if rec.crontab == crontab {
					continue
				}
				needDelete = true
			}

			needAdd = true
		case watch.Deleted:
			needDelete = true
		}

		if needDelete {
			if rec, ok := cronRecordMap[cronkey]; ok {
				c.Remove(rec.entryID)
				delete(cronRecordMap, cronkey)
			}
		}

		if needAdd {
			entryID, err := c.AddFunc(crontab, func() {
				err := event.wa.Patch(restartedAnnotation, time.Now().Format(time.RFC3339))
				if err != nil {
					log.Printf("run %s error: %s", cronkey, err)
				}
			})

			if err != nil {
				log.Printf("add cronjob error: %s", err)
			} else {
				cronRecordMap[cronkey] = cronRecord{
					entryID: entryID,
					crontab: crontab,
				}
			}
		}
	}

}

func makeKubeClient() (*kubernetes.Clientset, error) {
	kc, err := rest.InClusterConfig()

	if err != nil {
		kubeconfPath := os.Getenv("KUBECONFIG")
		if kubeconfPath == "" {
			kubeconfPath = os.Getenv("HOME") + "/.kube/config"
		}
		if err == rest.ErrNotInCluster {
			kc, err = clientcmd.BuildConfigFromFlags("", kubeconfPath)
		}
	}

	if kc == nil {
		return nil, err
	}

	return kubernetes.NewForConfig(kc)
}
