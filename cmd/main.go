// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;create;update
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"go.uber.org/zap/zapcore"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/discovery"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	monitoringv1alpha1 "github.com/fjsxy/monitor-in-hand/api/v1alpha1"
	ksmcontroller "github.com/fjsxy/monitor-in-hand/internal/controller"
	operatorruntime "github.com/fjsxy/monitor-in-hand/internal/operator"
	// +kubebuilder:scaffold:imports
)

var scheme = runtime.NewScheme()

type cliOptions struct {
	healthProbeBindAddress string
	logLevelText           string
	leaderElection         bool
}

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(monitoringv1alpha1.AddToScheme(scheme))

	// +kubebuilder:scaffold:scheme
}

func defaultCLIOptions() cliOptions {
	return cliOptions{
		healthProbeBindAddress: ":8081",
		logLevelText:           "info",
		leaderElection:         true,
	}
}

func (o *cliOptions) bindFlags(flags *flag.FlagSet) {
	flags.StringVar(&o.healthProbeBindAddress, "health-probe-bind-address", o.healthProbeBindAddress, "健康端点监听地址")
	flags.StringVar(&o.logLevelText, "log-level", o.logLevelText, "JSON 日志级别：debug、info、warn 或 error")
	flags.BoolVar(&o.leaderElection, "leader-elect", o.leaderElection, "启用 leader election；运行契约要求始终为 true")
}

func (o cliOptions) validate() error {
	if !o.leaderElection {
		return fmt.Errorf("--leader-elect=false 与控制面始终启用 leader election 的运行契约冲突")
	}
	return nil
}

func main() {
	options := defaultCLIOptions()
	options.bindFlags(flag.CommandLine)
	flag.Parse()

	if err := options.validate(); err != nil {
		writeFatalError(err)
		os.Exit(1)
	}
	if err := run(ctrl.SetupSignalHandler(), options.healthProbeBindAddress, options.logLevelText); err != nil {
		writeFatalError(err)
		os.Exit(1)
	}
}

func writeFatalError(err error) {
	_ = json.NewEncoder(os.Stderr).Encode(map[string]string{
		"level": "error",
		"msg":   "manager 退出",
		"error": err.Error(),
	})
}

func run(ctx context.Context, healthProbeBindAddress, logLevelText string) error {
	level := zapcore.InfoLevel
	if err := level.Set(logLevelText); err != nil {
		return fmt.Errorf("非法日志级别 %q: %w", logLevelText, err)
	}
	ctrl.SetLogger(zap.New(zap.UseDevMode(false), zap.JSONEncoder(), zap.Level(level)))
	klog.SetLogger(ctrl.Log.WithName("klog"))
	log := ctrl.Log.WithName("setup")

	config, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("读取 Kubernetes 配置: %w", err)
	}
	mgr, err := ctrl.NewManager(config, operatorruntime.ManagerOptions(scheme, healthProbeBindAddress))
	if err != nil {
		return fmt.Errorf("创建 manager: %w", err)
	}

	// +kubebuilder:scaffold:builder

	apiDiscovery, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return fmt.Errorf("创建 Kubernetes discovery client: %w", err)
	}
	if err := (&ksmcontroller.KubeStateMetricsReconciler{
		Client:        mgr.GetClient(),
		ServerVersion: apiDiscovery,
		Recorder:      mgr.GetEventRecorderFor("kubestatemetrics-controller"),
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("注册 KubeStateMetrics controller: %w", err)
	}
	participation := operatorruntime.NewParticipationState()
	if err := mgr.Add(participation); err != nil {
		return fmt.Errorf("注册 manager 参与状态标记: %w", err)
	}
	if err := mgr.AddHealthzCheck("process", healthz.Ping); err != nil {
		return fmt.Errorf("注册 liveness check: %w", err)
	}
	if err := mgr.AddReadyzCheck("control-plane", operatorruntime.NewReadyCheck(apiDiscovery, participation)); err != nil {
		return fmt.Errorf("注册 readiness check: %w", err)
	}

	log.Info("正在启动 manager", "leaderElectionID", operatorruntime.LeaderElectionID)
	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("运行 manager: %w", err)
	}
	return nil
}
