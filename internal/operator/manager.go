package operator

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

const LeaderElectionID = "monitor-in-hand.monitoring.fjsxy.github.io"

// ManagerOptions 是 Operator runtime 的唯一 manager 配置入口。
func ManagerOptions(scheme *runtime.Scheme, healthProbeBindAddress string) ctrl.Options {
	managedSelector := labels.SelectorFromSet(labels.Set{"app.kubernetes.io/managed-by": "monitor-in-hand"})
	managedTypes := []client.Object{
		&appsv1.Deployment{},
		&appsv1.StatefulSet{},
		&corev1.Service{},
		&corev1.ServiceAccount{},
		&rbacv1.ClusterRole{},
		&rbacv1.ClusterRoleBinding{},
		&rbacv1.Role{},
		&rbacv1.RoleBinding{},
	}
	managedCache := make(map[client.Object]cache.ByObject, len(managedTypes))
	for _, object := range managedTypes {
		managedCache[object] = cache.ByObject{Label: managedSelector}
	}

	return ctrl.Options{
		Scheme:                        scheme,
		LeaderElection:                true,
		LeaderElectionResourceLock:    "leases",
		LeaderElectionID:              LeaderElectionID,
		LeaderElectionReleaseOnCancel: true,
		Metrics: metricsserver.Options{
			BindAddress: "0",
		},
		HealthProbeBindAddress: healthProbeBindAddress,
		PprofBindAddress:       "0",
		// Owns informer 只观察本项目资源，未注册类型也不得按读请求动态扩张；精确身份 Get 走直读以保留无标签同名冲突检测。
		Cache:  cache.Options{ByObject: managedCache, ReaderFailOnMissingInformer: true},
		Client: client.Options{Cache: &client.CacheOptions{DisableFor: managedTypes}},
	}
}
