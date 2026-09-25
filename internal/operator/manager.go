package operator

import (
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

const LeaderElectionID = "monitor-in-hand.monitoring.fjsxy.github.io"

// ManagerOptions 是 Operator runtime 的唯一 manager 配置入口。
func ManagerOptions(scheme *runtime.Scheme, healthProbeBindAddress string) ctrl.Options {
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
	}
}
