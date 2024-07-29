Cron Restarter
==

kube-cron-restarter is a tool that allows you to schedule periodic restarts for your Kubernetes workloads to alleviate
issues caused by long-running applications such as memory leaks. This tool can be easily integrated with your Kubernetes
clusters by adding crontab-style annotations to your workloads.

### Getting started

Install kube-cron-restarter using Helm:

```shell
$ helm repo add kube-cron-restarter https://yxwuxuanl.github.io/kube-cron-restarter/
$ helm install kube-cron-restarter kube-cron-restarter/kube-cron-restarter \
  --set=timezone=Asia/Shanghai \
  --set=daemonset=true \
  --set=statefulset=true \
  --set=deployment=true

```

Create a deployment with the following annotation to schedule a restart at 00:00 every day:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app
  labels:
    app: my-app
  annotations:
    # Schedule a restart at 00:00 every day
    cron-restarter/schedule: '00 00 * * *'
spec:
  replicas: 1
  selector:
    matchLabels:
      app: my-app
  template:
    metadata:
      name: my-app
      labels:
        app: my-app
    spec:
      containers:
        - name: my-app
          image: nginx
          imagePullPolicy: IfNotPresent
          ports:
            - containerPort: 80
              protocol: TCP
      restartPolicy: Always
```