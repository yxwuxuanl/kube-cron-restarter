module kube-cron-restater

go 1.20

require (
	k8s.io/api v0.25.3
	k8s.io/apimachinery v0.25.3
	k8s.io/client-go v0.25.3
)

require github.com/robfig/cron/v3 v3.0.1 // indirect
