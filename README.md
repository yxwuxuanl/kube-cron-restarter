Cron Restarter
==

kube-cron-restarter 是一个 Kubernetes 的工具，它能够通过定时重启工作负载来缓解长时间运行导致的问题，如应用内存泄漏等。

kube-cron-restarter is a tool that allows you to schedule periodic restarts for your Kubernetes workloads to alleviate
issues caused by long-running applications such as memory leaks. This tool can be easily integrated with your Kubernetes
clusters by adding crontab-style annotations to your workloads.

### Getting started

你可以在你的 Kubernetes 工作负载上添加一个 crontab 格式的注解来指定 kube-cron-restarter 运行的时间。例如，以下注解将每天的凌晨
3 点重启 my-workload 工作负载：

To use kube-cron-restarter, you need to add the following annotations to your Kubernetes workloads:

```yaml
annotations:
    cron-restarter/cron: "0 3 * * *"
```

### Configuration

| Argument | Helm value |Documentation | Default |
| - | - | - | - |
| --deployment | restarter.deployment |enable deployments watch | true |
| --daemonset | restarter.daemonset |enable daemonsets watch | false |
| --statefulset | restarter.statefulset | enable statefulsets watch | false |
| --namespaces |restarter.namespaces|watch for namespaces | "" (all namespaces) |

| Env | Helm value | Documentation |
| - | - | - |
| TZ | restarter.env.TZ |Cron timezone |

### Deploy `kube-cron-restarter`

```shell
helm install kube-cron-restarter ./deploy/chart
```