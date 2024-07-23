Cron Restarter
==

kube-cron-restarter is a tool that allows you to schedule periodic restarts for your Kubernetes workloads to alleviate
issues caused by long-running applications such as memory leaks. This tool can be easily integrated with your Kubernetes
clusters by adding crontab-style annotations to your workloads.

### Getting started
```shell
helm install kube-cron-restarter ./helm/kube-cron-restarter
```