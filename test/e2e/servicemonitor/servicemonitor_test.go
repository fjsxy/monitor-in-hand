package servicemonitor_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	api "github.com/fjsxy/monitor-in-hand/api/v1alpha1"
	ksm "github.com/fjsxy/monitor-in-hand/internal/kubestatemetrics"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	extclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

func TestServiceMonitor(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Operator 配套 ServiceMonitor 真实集群")
}

var ids = map[string][]string{
	"1.19": {"E2E-119-SM-GH86-001", "E2E-119-SM-GH86-002", "E2E-119-SM-GH86-003", "E2E-119-SM-GH86-004", "E2E-119-SM-GH86-005", "E2E-119-SM-GH86-006", "E2E-119-SM-GH86-007"},
	"1.28": {"E2E-128-SM-GH86-001", "E2E-128-SM-GH86-002", "E2E-128-SM-GH86-003", "E2E-128-SM-GH86-004", "E2E-128-SM-GH86-005", "E2E-128-SM-GH86-006", "E2E-128-SM-GH86-007"},
}

func id(n int) string {
	v := ids[os.Getenv("KUBERNETES_VERSION")]
	if len(v) < n {
		return "unsupported"
	}
	return v[n-1]
}

const ns = "monitor-in-hand-test"

var dc dynamic.Interface
var kc kubernetes.Interface
var ec extclient.Interface
var cc client.Client
var root, kubeContext, minor string
var names []string
var peerUID string
var peerspec map[string]interface{}
var external map[string]map[string]interface{}
var smGVR = schema.GroupVersionResource{Group: "monitoring.coreos.com", Version: "v1", Resource: "servicemonitors"}
var agentGVR = schema.GroupVersionResource{Group: "monitoring.coreos.com", Version: "v1alpha1", Resource: "prometheusagents"}

var _ = BeforeSuite(func() {
	By("仅使用带本轮身份标记的独立 kind 集群")
	minor = os.Getenv("KUBERNETES_VERSION")
	Expect(ids).To(HaveKey(minor))
	runID := os.Getenv("TEST_RUN_ID")
	Expect(runID).To(MatchRegexp(`^[a-z0-9-]+$`))
	kubeContext = "kind-mih-e2e-" + strings.ReplaceAll(minor, ".", "") + "-" + runID
	var err error
	root, err = filepath.Abs("../../..")
	Expect(err).NotTo(HaveOccurred())
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{CurrentContext: kubeContext}).ClientConfig()
	Expect(err).NotTo(HaveOccurred())
	kc, err = kubernetes.NewForConfig(config)
	Expect(err).NotTo(HaveOccurred())
	dc, err = dynamic.NewForConfig(config)
	Expect(err).NotTo(HaveOccurred())
	ec, err = extclient.NewForConfig(config)
	Expect(err).NotTo(HaveOccurred())
	scheme := runtime.NewScheme()
	Expect(api.AddToScheme(scheme)).To(Succeed())
	cc, err = client.New(config, client.Options{Scheme: scheme})
	Expect(err).NotTo(HaveOccurred())
	marker, err := kc.CoreV1().ConfigMaps("kube-system").Get(context.Background(), "monitor-in-hand-test-run", metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	Expect(marker.Data["testRunID"]).To(Equal(runID))
	Expect(marker.Data["kubernetesVersion"]).To(Equal(minor))
	external = map[string]map[string]interface{}{}
	for _, r := range []schema.GroupVersionResource{agentGVR, {Group: "monitoring.coreos.com", Version: "v1", Resource: "podmonitors"}, {Group: "monitoring.coreos.com", Version: "v1", Resource: "probes"}, {Group: "monitoring.coreos.com", Version: "v1alpha1", Resource: "scrapeconfigs"}} {
		list, e := dc.Resource(r).Namespace(ns).List(context.Background(), metav1.ListOptions{})
		Expect(e).NotTo(HaveOccurred())
		for _, obj := range list.Items {
			external[r.Resource+"/"+obj.GetName()] = map[string]interface{}{"uid": string(obj.GetUID()), "spec": obj.Object["spec"]}
		}
	}
})
var _ = AfterSuite(func(ctx SpecContext) {
	By("先清理 Agent，再等待全部本轮 KSM finalizer 完成")
	if dc == nil {
		return
	}
	p := metav1.DeletePropagationForeground
	Expect(dc.Resource(agentGVR).Namespace(ns).Delete(ctx, "gh86", metav1.DeleteOptions{PropagationPolicy: &p})).To(SatisfyAny(Succeed(), WithTransform(apierrors.IsNotFound, BeTrue())))
	Eventually(func() bool {
		_, e := dc.Resource(agentGVR).Namespace(ns).Get(ctx, "gh86", metav1.GetOptions{})
		return apierrors.IsNotFound(e)
	}, 180*time.Second, time.Second).Should(BeTrue())
	for _, name := range names {
		obj := &api.KubeStateMetrics{}
		err := cc.Get(ctx, client.ObjectKey{Name: name}, obj)
		if apierrors.IsNotFound(err) {
			continue
		}
		Expect(err).NotTo(HaveOccurred())
		Expect(cc.Delete(ctx, obj)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(cc.Get(ctx, client.ObjectKey{Name: name}, &api.KubeStateMetrics{}))
		}, 120*time.Second, time.Second).Should(BeTrue())
		_, err = dc.Resource(smGVR).Namespace(ns).Get(ctx, ksm.ManagedResourceName(name), metav1.GetOptions{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	}
	Expect(kc.CoreV1().Pods(ns).Delete(ctx, "gh86-unscheduled", metav1.DeleteOptions{})).To(SatisfyAny(Succeed(), WithTransform(apierrors.IsNotFound, BeTrue())))
	By("外部 Agent 与独立采集配置的 UID/spec 不变")
	for key, expected := range external {
		parts := strings.SplitN(key, "/", 2)
		version := "v1"
		if parts[0] == "prometheusagents" || parts[0] == "scrapeconfigs" {
			version = "v1alpha1"
		}
		obj, err := dc.Resource(schema.GroupVersionResource{Group: "monitoring.coreos.com", Version: version, Resource: parts[0]}).Namespace(ns).Get(ctx, parts[1], metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(string(obj.GetUID())).To(Equal(expected["uid"]))
		Expect(obj.Object["spec"]).To(Equal(expected["spec"]))
	}
}, NodeTimeout(10*time.Minute))

var _ = Describe("配套采集配置", Ordered, func() {
	It(id(1)+" 缺 API 启动与晚装 CRD 恢复", func(ctx SpecContext) {
		By("测试准备主动移除仅属于本隔离环境的 SM CRD，然后 Helm 安装当前构建")
		Expect(ec.ApiextensionsV1().CustomResourceDefinitions().Delete(ctx, "servicemonitors.monitoring.coreos.com", metav1.DeleteOptions{})).To(Succeed())
		Eventually(func() bool {
			_, e := ec.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, "servicemonitors.monitoring.coreos.com", metav1.GetOptions{})
			return apierrors.IsNotFound(e)
		}, 90*time.Second, time.Second).Should(BeTrue())
		_, err := kc.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "monitor-in-hand-system"}}, metav1.CreateOptions{})
		Expect(err).To(SatisfyAny(Succeed(), WithTransform(apierrors.IsAlreadyExists, BeTrue())))
		command(ctx, env("HELM_BIN", "helm"), "upgrade", "--install", "monitor-in-hand", filepath.Join(root, "charts/monitor-in-hand"), "--kube-context", kubeContext, "--namespace", "monitor-in-hand-system", "--wait", "--timeout", "180s")
		create(ctx, "gh86-legacy", false)
		create(ctx, "gh86-main", true)
		waitWorkload(ctx, "gh86-legacy")
		waitCR(ctx, "gh86-main", func(c *api.KubeStateMetrics) bool {
			return c.Status.ServiceMonitor != nil && c.Status.ServiceMonitor.Reason == "DependencyMissing"
		})
		before := getCR(ctx, "gh86-main")
		By("安装官方锁定 CRD，不更新原 CR，等待自动收敛")
		raw, err := os.ReadFile(filepath.Join(root, "test/fixtures/servicemonitor/monitoring.coreos.com_servicemonitors.yaml"))
		Expect(err).NotTo(HaveOccurred())
		def := &extv1.CustomResourceDefinition{}
		Expect(yaml.Unmarshal(raw, def)).To(Succeed())
		_, err = ec.ApiextensionsV1().CustomResourceDefinitions().Create(ctx, def, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		waitMonitor(ctx, "gh86-main")
		after := getCR(ctx, "gh86-main")
		Expect(after.UID).To(Equal(before.UID))
		Expect(after.Generation).To(Equal(before.Generation))
		waitWorkload(ctx, "gh86-main")
	}, NodeTimeout(8*time.Minute))
	It(id(2)+" 默认参数、长名称与多实例隔离", func(ctx SpecContext) {
		By("创建长名称对照实例并回读真实对象身份")
		peer := "gh86.peer." + strings.Repeat("long", 15)
		create(ctx, peer, true)
		update(ctx, peer, func(c *api.KubeStateMetrics) { c.Spec.ServiceMonitor.Labels["monitor-mgr.io/user-id"] = "other" })
		waitMonitor(ctx, peer)
		for _, name := range []string{"gh86-main", peer} {
			obj := getMonitor(ctx, name)
			cr := getCR(ctx, name)
			Expect(obj.GetOwnerReferences()[0].UID).To(Equal(cr.UID))
			Expect(obj.GetNamespace()).To(Equal(ns))
			eps := obj.Object["spec"].(map[string]interface{})["endpoints"].([]interface{})
			Expect(eps).To(ConsistOf(HaveKeyWithValue("port", "http-metrics")))
			Expect(eps[0]).To(HaveKeyWithValue("interval", "30s"))
			Expect(cr.Status.ServiceMonitor.Resource.UID).To(Equal(string(obj.GetUID())))
		}
		obj := getMonitor(ctx, peer)
		peerUID = string(obj.GetUID())
		peerspec = obj.Object["spec"].(map[string]interface{})
		_, err := dc.Resource(smGVR).Namespace(ns).Get(ctx, ksm.ManagedResourceName("gh86-legacy"), metav1.GetOptions{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	}, NodeTimeout(6*time.Minute))
	It(id(3)+" 漂移重建、权限故障及重启恢复", func(ctx SpecContext) {
		By("修改配套 spec 后自动修复；删除后以新 UID 重建")
		obj := getMonitor(ctx, "gh86-main")
		oldUID := obj.GetUID()
		obj.Object["spec"].(map[string]interface{})["endpoints"] = []interface{}{map[string]interface{}{"port": "wrong"}}
		_, err := dc.Resource(smGVR).Namespace(ns).Update(ctx, obj, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() string {
			o := getMonitor(ctx, "gh86-main")
			return o.Object["spec"].(map[string]interface{})["endpoints"].([]interface{})[0].(map[string]interface{})["port"].(string)
		}, 40*time.Second, time.Second).Should(Equal("http-metrics"))
		Expect(dc.Resource(smGVR).Namespace(ns).Delete(ctx, obj.GetName(), metav1.DeleteOptions{})).To(Succeed())
		waitCR(ctx, "gh86-main", func(c *api.KubeStateMetrics) bool {
			return c.Status.ServiceMonitor != nil && c.Status.ServiceMonitor.Resource != nil && c.Status.ServiceMonitor.Resource.UID != "" && c.Status.ServiceMonitor.Resource.UID != string(oldUID)
		})
		By("撤销 Operator SM 权限并修改意图，故障期保留 workload")
		role, err := kc.RbacV1().ClusterRoles().Get(ctx, "monitor-in-hand-manager-role", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		original := append([]rbacv1.PolicyRule(nil), role.Rules...)
		role.Rules = nil
		for _, rule := range original {
			if !contains(rule.APIGroups, "monitoring.coreos.com") {
				role.Rules = append(role.Rules, rule)
			}
		}
		_, err = kc.RbacV1().ClusterRoles().Update(ctx, role, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		restore := func() {
			r, e := kc.RbacV1().ClusterRoles().Get(context.Background(), role.Name, metav1.GetOptions{})
			Expect(e).NotTo(HaveOccurred())
			r.Rules = original
			_, e = kc.RbacV1().ClusterRoles().Update(context.Background(), r, metav1.UpdateOptions{})
			Expect(e).NotTo(HaveOccurred())
		}
		DeferCleanup(restore)
		update(ctx, "gh86-main", func(c *api.KubeStateMetrics) { c.Spec.ServiceMonitor.Interval = "20s" })
		waitCR(ctx, "gh86-main", func(c *api.KubeStateMetrics) bool {
			return c.Status.ServiceMonitor != nil && c.Status.ServiceMonitor.Reason == "PermissionDenied"
		})
		waitWorkload(ctx, "gh86-main")
		restore()
		By("重启当前 Operator 后不重发 CR 即收敛")
		deployment, err := kc.AppsV1().Deployments("monitor-in-hand-system").Get(ctx, "monitor-in-hand-monitor-in-hand", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		if deployment.Spec.Template.Annotations == nil {
			deployment.Spec.Template.Annotations = map[string]string{}
		}
		deployment.Spec.Template.Annotations["gh86/restart"] = time.Now().UTC().Format(time.RFC3339Nano)
		_, err = kc.AppsV1().Deployments("monitor-in-hand-system").Update(ctx, deployment, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		waitMonitor(ctx, "gh86-main")
	}, NodeTimeout(8*time.Minute))
	It(id(4)+" 外部同名和旧 owner 不接管", func(ctx SpecContext) {
		By("准备不带管理标签的同名独立对象")
		obj := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "monitoring.coreos.com/v1", "kind": "ServiceMonitor", "metadata": map[string]interface{}{"name": "ksm-gh86-conflict", "namespace": ns}, "spec": map[string]interface{}{"selector": map[string]interface{}{}, "endpoints": []interface{}{map[string]interface{}{"port": "foreign"}}}}}
		foreign, err := dc.Resource(smGVR).Namespace(ns).Create(ctx, obj, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		create(ctx, "gh86-conflict", true)
		waitCR(ctx, "gh86-conflict", func(c *api.KubeStateMetrics) bool {
			return c.Status.ServiceMonitor != nil && c.Status.ServiceMonitor.Reason == "ResourceConflict"
		})
		Consistently(func() bool {
			o, e := dc.Resource(smGVR).Namespace(ns).Get(ctx, foreign.GetName(), metav1.GetOptions{})
			return e == nil && o.GetUID() == foreign.GetUID() && reflect.DeepEqual(o.Object["spec"], foreign.Object["spec"]) && reflect.DeepEqual(o.GetLabels(), foreign.GetLabels())
		}, 6*time.Second, time.Second).Should(BeTrue())
		By("旧 CR UID 即使带管理标签也不能接管；finalizer 仅用于防止测试夹具被 GC 抢先删除")
		foreign, err = dc.Resource(smGVR).Namespace(ns).Get(ctx, foreign.GetName(), metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		yes := true
		foreign.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "KubeStateMetrics", Name: "gh86-conflict", UID: types.UID("previous-cr-uid"), Controller: &yes}})
		foreign.SetLabels(map[string]string{"app.kubernetes.io/managed-by": "monitor-in-hand", "foreign": "preserve"})
		foreign.SetFinalizers([]string{"gh86.test/fixture"})
		foreign, err = dc.Resource(smGVR).Namespace(ns).Update(ctx, foreign, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Consistently(func() bool {
			o, e := dc.Resource(smGVR).Namespace(ns).Get(ctx, foreign.GetName(), metav1.GetOptions{})
			return e == nil && o.GetUID() == foreign.GetUID() && reflect.DeepEqual(o.Object["spec"], foreign.Object["spec"]) && reflect.DeepEqual(o.GetLabels(), foreign.GetLabels())
		}, 8*time.Second, time.Second).Should(BeTrue())
		Expect(getCR(ctx, "gh86-conflict").Status.ServiceMonitor.Reason).To(Equal("ResourceConflict"))
		By("冲突解除后自动创建当前 owner 的对象")
		foreign, err = dc.Resource(smGVR).Namespace(ns).Get(ctx, foreign.GetName(), metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		foreign.SetFinalizers(nil)
		_, err = dc.Resource(smGVR).Namespace(ns).Update(ctx, foreign, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(dc.Resource(smGVR).Namespace(ns).Delete(ctx, foreign.GetName(), metav1.DeleteOptions{})).To(SatisfyAny(Succeed(), WithTransform(apierrors.IsNotFound, BeTrue())))
		waitMonitor(ctx, "gh86-conflict")
	}, NodeTimeout(4*time.Minute))
	It(id(5)+" 挂起、关闭采集、停用、恢复与删除", func(ctx SpecContext) {
		create(ctx, "gh86-life", true)
		waitMonitor(ctx, "gh86-life")
		waitWorkload(ctx, "gh86-life")
		before := getMonitor(ctx, "gh86-life").GetUID()
		By("挂起保留原配套对象；关闭只清理配套对象")
		update(ctx, "gh86-life", func(c *api.KubeStateMetrics) { b := true; c.Spec.Suspended = &b })
		waitCR(ctx, "gh86-life", func(c *api.KubeStateMetrics) bool {
			return c.Status.DesiredPods == 0 && c.Status.ObservedGeneration == c.Generation
		})
		Expect(getMonitor(ctx, "gh86-life").GetUID()).To(Equal(before))
		update(ctx, "gh86-life", func(c *api.KubeStateMetrics) {
			b := false
			c.Spec.Suspended = &b
			c.Spec.ServiceMonitor.Enabled = false
		})
		waitCR(ctx, "gh86-life", func(c *api.KubeStateMetrics) bool {
			return c.Status.ServiceMonitor != nil && c.Status.ServiceMonitor.State == "Disabled"
		})
		waitWorkload(ctx, "gh86-life")
		By("重新开启、停用清理和恢复都只影响本实例")
		update(ctx, "gh86-life", func(c *api.KubeStateMetrics) { c.Spec.ServiceMonitor.Enabled = true })
		waitMonitor(ctx, "gh86-life")
		update(ctx, "gh86-life", func(c *api.KubeStateMetrics) { b := false; c.Spec.Enabled = &b })
		waitCR(ctx, "gh86-life", func(c *api.KubeStateMetrics) bool { return c.Status.Phase == api.KubeStateMetricsPhaseDisabled })
		_, err := dc.Resource(smGVR).Namespace(ns).Get(ctx, "ksm-gh86-life", metav1.GetOptions{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
		update(ctx, "gh86-life", func(c *api.KubeStateMetrics) { b := true; c.Spec.Enabled = &b })
		waitMonitor(ctx, "gh86-life")
		cr := getCR(ctx, "gh86-life")
		Expect(cc.Delete(ctx, cr)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(cc.Get(ctx, client.ObjectKey{Name: cr.Name}, &api.KubeStateMetrics{}))
		}, 120*time.Second, time.Second).Should(BeTrue())
		checkPeer(ctx)
	}, NodeTimeout(8*time.Minute))
	It(id(6)+" 所有合法拓扑经原生 Agent 产生真实指标", func(ctx SpecContext) {
		By("创建分片实例并通过当前 Operator 自动配套")
		create(ctx, "gh86-sharded", true)
		update(ctx, "gh86-sharded", func(c *api.KubeStateMetrics) {
			c.Spec.Deployment = api.KubeStateMetricsDeploymentSpec{Mode: api.DeploymentModeStatefulSetSharded, StatefulSetSharded: &api.KubeStateMetricsStatefulSetShardedSpec{Shards: 2}}
		})
		if minor == "1.28" {
			create(ctx, "gh86-daemon", true)
			update(ctx, "gh86-daemon", func(c *api.KubeStateMetrics) {
				c.Spec.Deployment = api.KubeStateMetricsDeploymentSpec{Mode: api.DeploymentModeDaemonSetSharded, DaemonSetSharded: &api.KubeStateMetricsDaemonSetShardedSpec{}}
			})
		} else {
			unsupported := &api.KubeStateMetrics{ObjectMeta: metav1.ObjectMeta{Name: "gh86-unsupported"}, Spec: api.KubeStateMetricsSpec{TargetNamespace: ns, ServiceMonitor: &api.KubeStateMetricsServiceMonitor{Enabled: true}, Deployment: api.KubeStateMetricsDeploymentSpec{Mode: api.DeploymentModeDaemonSetSharded, DaemonSetSharded: &api.KubeStateMetricsDaemonSetShardedSpec{}}}}
			Expect(cc.Create(ctx, unsupported)).To(Succeed())
			names = append(names, unsupported.Name)
			waitCR(ctx, "gh86-unsupported", func(c *api.KubeStateMetrics) bool {
				for _, condition := range c.Status.Conditions {
					if condition.Reason == "UnsupportedMode" {
						return true
					}
				}
				return false
			})
			By("1.19 不支持模式在首次声明后没有任何受管资源写入")
			for _, gvr := range []schema.GroupVersionResource{smGVR, {Version: "v1", Resource: "services"}, {Version: "v1", Resource: "serviceaccounts"}, {Group: "apps", Version: "v1", Resource: "deployments"}, {Group: "apps", Version: "v1", Resource: "statefulsets"}, {Group: "apps", Version: "v1", Resource: "daemonsets"}, {Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}, {Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}} {
				objects, err := dc.Resource(gvr).Namespace(ns).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/instance=gh86-unsupported"})
				Expect(err).NotTo(HaveOccurred())
				Expect(objects.Items).To(BeEmpty(), gvr.String())
			}
			for _, resource := range []string{"clusterroles", "clusterrolebindings"} {
				objects, err := dc.Resource(schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: resource}).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/instance=gh86-unsupported"})
				Expect(err).NotTo(HaveOccurred())
				Expect(objects.Items).To(BeEmpty())
			}
		}
		By("保留一个未调度 Pod，使 DaemonSet 的 unscheduled 端点也有实际 Pod 样本")
		_, err := kc.CoreV1().Pods(ns).Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "gh86-unscheduled"}, Spec: corev1.PodSpec{SchedulerName: "gh86-no-scheduler", Containers: []corev1.Container{{Name: "placeholder", Image: "registry.k8s.io/pause:3.9"}}}}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		for _, name := range []string{"gh86-main", "gh86-sharded"} {
			update(ctx, name, func(c *api.KubeStateMetrics) {
				c.Spec.ServiceMonitor.Interval = "5s"
				c.Spec.ServiceMonitor.ScrapeTimeout = "3s"
			})
			waitMonitor(ctx, name)
			waitWorkload(ctx, name)
		}
		if minor == "1.28" {
			update(ctx, "gh86-daemon", func(c *api.KubeStateMetrics) {
				c.Spec.ServiceMonitor.Interval = "5s"
				c.Spec.ServiceMonitor.ScrapeTimeout = "3s"
			})
			waitMonitor(ctx, "gh86-daemon")
			waitWorkload(ctx, "gh86-daemon")
		}
		By("原生 Agent 选择 Operator 生成对象，remote_write 到独立 tenant")
		agent, err := dc.Resource(agentGVR).Namespace(ns).Get(ctx, "fixture", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		agent.SetName("gh86")
		agent.SetUID("")
		agent.SetResourceVersion("")
		agent.SetGeneration(0)
		agent.SetManagedFields(nil)
		delete(agent.Object, "status")
		spec := agent.Object["spec"].(map[string]interface{})
		for _, k := range []string{"podMonitorSelector", "probeSelector", "scrapeConfigSelector"} {
			delete(spec, k)
		}
		spec["serviceMonitorSelector"] = map[string]interface{}{"matchLabels": map[string]interface{}{"monitor-mgr.io/user-id": "user-86"}}
		spec["remoteWrite"] = []interface{}{map[string]interface{}{"url": "http://vminsert.monitor-in-hand-test.svc:8480/insert/86/prometheus/api/v1/write"}}
		_, err = dc.Resource(agentGVR).Namespace(ns).Create(ctx, agent, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		assertTargetsAndSamples(ctx)
	}, NodeTimeout(12*time.Minute))
	It(id(7)+" 零 namespace 与模式切换保持正确目标集合", func(ctx SpecContext) {
		By("namespace 零匹配时保留配套对象，恢复后继续采集")
		update(ctx, "gh86-main", func(c *api.KubeStateMetrics) {
			c.Spec.NamespaceFilter = api.NamespaceFilter{Mode: api.NamespaceFilterModeInclude, NamePatterns: []string{"^gh86-no-match$"}}
		})
		waitCR(ctx, "gh86-main", func(c *api.KubeStateMetrics) bool {
			return c.Status.ObservedGeneration == c.Generation && c.Status.EffectiveNamespaceCount == 0 && c.Status.DesiredPods == 0
		})
		Expect(getMonitor(ctx, "gh86-main")).NotTo(BeNil())
		update(ctx, "gh86-main", func(c *api.KubeStateMetrics) {
			c.Spec.NamespaceFilter = api.NamespaceFilter{Mode: api.NamespaceFilterModeAll}
			c.Spec.Deployment = api.KubeStateMetricsDeploymentSpec{Mode: api.DeploymentModeStatefulSetSharded, StatefulSetSharded: &api.KubeStateMetricsStatefulSetShardedSpec{Shards: 2}}
		})
		waitWorkload(ctx, "gh86-main")
		waitMonitor(ctx, "gh86-main")
		assertTargetsAndSamples(ctx)
		if minor == "1.28" {
			By("StatefulSet 切换 DaemonSet 后同时验证 node/control 目标与旧 workload 消失")
			update(ctx, "gh86-main", func(c *api.KubeStateMetrics) {
				c.Spec.Deployment = api.KubeStateMetricsDeploymentSpec{Mode: api.DeploymentModeDaemonSetSharded, DaemonSetSharded: &api.KubeStateMetricsDaemonSetShardedSpec{}}
			})
			waitWorkload(ctx, "gh86-main")
			waitMonitor(ctx, "gh86-main")
			_, err := kc.AppsV1().StatefulSets(ns).Get(ctx, "ksm-gh86-main", metav1.GetOptions{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
			_, err = kc.AppsV1().Deployments(ns).Get(ctx, "ksm-gh86-main", metav1.GetOptions{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
			assertTargetsAndSamples(ctx)
		}
		checkPeer(ctx)
	}, NodeTimeout(10*time.Minute))
})

// 下面的 helper 只通过 Kubernetes/Agent/查询 API 观察公开行为，异步均有上限。
func create(ctx context.Context, name string, enabled bool) {
	GinkgoHelper()
	c := &api.KubeStateMetrics{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: api.KubeStateMetricsSpec{TargetNamespace: ns}}
	if enabled {
		c.Spec.ServiceMonitor = &api.KubeStateMetricsServiceMonitor{Enabled: true, Labels: map[string]string{"monitor-mgr.io/user-id": "user-86"}}
	}
	Expect(cc.Create(ctx, c)).To(Succeed())
	names = append(names, name)
}
func getCR(ctx context.Context, name string) *api.KubeStateMetrics {
	GinkgoHelper()
	c := &api.KubeStateMetrics{}
	Expect(cc.Get(ctx, client.ObjectKey{Name: name}, c)).To(Succeed())
	return c
}
func update(ctx context.Context, name string, change func(*api.KubeStateMetrics)) {
	GinkgoHelper()
	Eventually(func() error {
		c := &api.KubeStateMetrics{}
		if e := cc.Get(ctx, client.ObjectKey{Name: name}, c); e != nil {
			return e
		}
		change(c)
		return cc.Update(ctx, c)
	}, 30*time.Second, time.Second).Should(Succeed())
}
func waitCR(ctx context.Context, name string, predicate func(*api.KubeStateMetrics) bool) {
	GinkgoHelper()
	Eventually(func() bool {
		c := &api.KubeStateMetrics{}
		if e := cc.Get(ctx, client.ObjectKey{Name: name}, c); e != nil {
			return false
		}
		return predicate(c)
	}, 180*time.Second, time.Second).Should(BeTrue(), name)
}
func waitMonitor(ctx context.Context, name string) {
	GinkgoHelper()
	waitCR(ctx, name, func(c *api.KubeStateMetrics) bool {
		return c.Status.ServiceMonitor != nil && c.Status.ServiceMonitor.State == "Ready" && c.Status.ServiceMonitor.ObservedGeneration == c.Generation
	})
}
func waitWorkload(ctx context.Context, name string) {
	GinkgoHelper()
	waitCR(ctx, name, func(c *api.KubeStateMetrics) bool {
		return c.Status.ObservedGeneration == c.Generation && c.Status.AvailablePods > 0 && c.Status.AvailablePods >= c.Status.DesiredPods
	})
}
func getMonitor(ctx context.Context, name string) *unstructured.Unstructured {
	GinkgoHelper()
	o, e := dc.Resource(smGVR).Namespace(ns).Get(ctx, ksm.ManagedResourceName(name), metav1.GetOptions{})
	Expect(e).NotTo(HaveOccurred())
	return o
}
func checkPeer(ctx context.Context) {
	GinkgoHelper()
	for _, name := range names {
		if strings.HasPrefix(name, "gh86.peer.") {
			o := getMonitor(ctx, name)
			Expect(string(o.GetUID())).To(Equal(peerUID))
			Expect(o.Object["spec"]).To(Equal(peerspec))
		}
	}
}
func contains(a []string, v string) bool {
	for _, s := range a {
		if s == v {
			return true
		}
	}
	return false
}
func env(k, d string) string {
	if s := os.Getenv(k); s != "" {
		return s
	}
	return d
}
func command(ctx context.Context, name string, args ...string) string {
	GinkgoHelper()
	c := exec.CommandContext(ctx, name, args...)
	c.Dir = root
	b, e := c.CombinedOutput()
	Expect(e).NotTo(HaveOccurred(), string(b))
	return string(b)
}
func forward(ctx context.Context, target string, port int) (string, func()) {
	GinkgoHelper()
	l, e := net.Listen("tcp", "127.0.0.1:0")
	Expect(e).NotTo(HaveOccurred())
	local := l.Addr().(*net.TCPAddr).Port
	l.Close()
	c := exec.CommandContext(ctx, env("KUBECTL", "kubectl"), "--context", kubeContext, "-n", ns, "port-forward", target, fmt.Sprintf("%d:%d", local, port))
	Expect(c.Start()).To(Succeed())
	stop := func() { _ = c.Process.Kill(); _ = c.Wait() }
	Eventually(func() error {
		conn, e := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", local), time.Second)
		if e == nil {
			conn.Close()
		}
		return e
	}, 30*time.Second, time.Second).Should(Succeed())
	return fmt.Sprintf("http://127.0.0.1:%d", local), stop
}
func readHTTP(ctx context.Context, u string) ([]byte, error) {
	r, e := http.NewRequestWithContext(ctx, "GET", u, nil)
	if e != nil {
		return nil, e
	}
	resp, e := (&http.Client{Timeout: 5 * time.Second}).Do(r)
	if e != nil {
		return nil, e
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}
func assertTargetsAndSamples(ctx context.Context) {
	GinkgoHelper()
	By("读取真实 Agent targets 并逐一比对必要 Service endpoint")
	Eventually(func() bool {
		p, e := kc.CoreV1().Pods(ns).Get(ctx, "prom-agent-gh86-0", metav1.GetOptions{})
		if e != nil {
			return false
		}
		for _, s := range p.Status.ContainerStatuses {
			if s.Name == "prometheus" {
				return s.Ready
			}
		}
		return false
	}, 180*time.Second, time.Second).Should(BeTrue())
	base, stop := forward(ctx, "pod/prom-agent-gh86-0", 9090)
	defer stop()
	Eventually(func() error {
		b, e := readHTTP(ctx, base+"/api/v1/targets")
		if e != nil {
			return e
		}
		var response struct {
			Data struct {
				ActiveTargets []struct {
					Health     string            `json:"health"`
					ScrapeURL  string            `json:"scrapeUrl"`
					Discovered map[string]string `json:"discoveredLabels"`
				} `json:"activeTargets"`
			} `json:"data"`
		}
		if e = json.Unmarshal(b, &response); e != nil {
			return e
		}
		expected, e := expectedEndpoints(ctx)
		if e != nil {
			return e
		}
		actual := map[string]bool{}
		for _, t := range response.Data.ActiveTargets {
			if t.Health != "up" {
				return fmt.Errorf("target 尚未 up")
			}
			u, e := url.Parse(t.ScrapeURL)
			if e != nil {
				return e
			}
			actual[t.Discovered["__meta_kubernetes_service_name"]+"|"+u.Host] = true
		}
		if len(actual) != len(expected) {
			return fmt.Errorf("实际 targets=%v，必要端点=%v", actual, expected)
		}
		for key := range expected {
			if !actual[key] {
				return fmt.Errorf("Agent 缺少必要 target %s", key)
			}
		}
		AddReportEntry("真实targets", string(b), ReportEntryVisibilityFailureOrVerbose)
		return nil
	}, 180*time.Second, 2*time.Second).Should(Succeed())
	By("从 remote_write 接收端逐一核验必要端点的 up 与职责对应的指标样本")
	queryBase, queryStop := forward(ctx, "service/vmselect", 8481)
	defer queryStop()
	query := url.QueryEscape(`count by (__name__,service,instance) ({__name__=~"up|kube_pod_info|kube_namespace_created",prometheus="monitor-in-hand-test/gh86"})`)
	Eventually(func() error {
		b, e := readHTTP(ctx, queryBase+"/select/86/prometheus/api/v1/query?query="+query)
		if e != nil {
			return e
		}
		var v struct {
			Status string `json:"status"`
			Data   struct {
				Result []struct {
					Metric map[string]string `json:"metric"`
				} `json:"result"`
			} `json:"data"`
		}
		if e := json.Unmarshal(b, &v); e != nil {
			return e
		}
		if v.Status != "success" {
			return fmt.Errorf("查询未成功: %s", string(b))
		}
		observed := map[string]bool{}
		for _, sample := range v.Data.Result {
			observed[sample.Metric["service"]+"|"+sample.Metric["instance"]+"|"+sample.Metric["__name__"]] = true
		}
		expected, e := expectedEndpoints(ctx)
		if e != nil {
			return e
		}
		for endpoint, family := range expected {
			for _, metric := range []string{"up", family} {
				if !observed[endpoint+"|"+metric] {
					return fmt.Errorf("remote_write 缺少 %s 的 %s 样本；已有 %v", endpoint, metric, observed)
				}
			}
		}
		AddReportEntry("真实remote_write样本", string(b), ReportEntryVisibilityFailureOrVerbose)
		return nil
	}, 180*time.Second, 2*time.Second).Should(Succeed())
}

// expectedEndpoints 从声明的拓扑确定必要 Service 和数量，不能从现存 Service 反推而漏掉缺失对象。
func expectedEndpoints(ctx context.Context) (map[string]string, error) {
	expected := map[string]string{}
	for _, name := range []string{"gh86-main", "gh86-conflict", "gh86-sharded", "gh86-daemon"} {
		if name == "gh86-daemon" && minor != "1.28" {
			continue
		}
		cr := &api.KubeStateMetrics{}
		if err := cc.Get(ctx, client.ObjectKey{Name: name}, cr); err != nil {
			return nil, err
		}
		base := "ksm-" + name
		counts := map[string]int{base: 1}
		if cr.Spec.Deployment.Mode == api.DeploymentModeStatefulSetSharded {
			counts[base] = 2
		}
		if cr.Spec.Deployment.Mode == api.DeploymentModeDaemonSetSharded {
			ds, err := kc.AppsV1().DaemonSets(ns).Get(ctx, base, metav1.GetOptions{})
			if err != nil {
				return nil, err
			}
			if ds.Status.DesiredNumberScheduled < 1 {
				return nil, fmt.Errorf("%s 没有 node 端点", base)
			}
			counts[base] = int(ds.Status.DesiredNumberScheduled)
			counts[base+"-control"] = 2
		}
		services, err := kc.CoreV1().Services(ns).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/instance=" + name})
		if err != nil {
			return nil, err
		}
		if len(services.Items) != len(counts) {
			return nil, fmt.Errorf("%s Service 数量 %d，预期 %d", name, len(services.Items), len(counts))
		}
		for service, count := range counts {
			ep, err := kc.CoreV1().Endpoints(ns).Get(ctx, service, metav1.GetOptions{})
			if err != nil {
				return nil, err
			}
			seen, roles := 0, map[string]int{}
			for _, subset := range ep.Subsets {
				for _, address := range subset.Addresses {
					if address.TargetRef == nil {
						return nil, fmt.Errorf("%s endpoint 缺少 Pod 引用", service)
					}
					pod, err := kc.CoreV1().Pods(ns).Get(ctx, address.TargetRef.Name, metav1.GetOptions{})
					if err != nil {
						return nil, err
					}
					role := pod.Labels["monitor-in-hand/role"]
					roles[role]++
					family := "kube_pod_info"
					if role == "cluster" {
						family = "kube_namespace_created"
					}
					expected[service+"|"+address.IP+":8080"] = family
					seen++
				}
			}
			if seen != count {
				return nil, fmt.Errorf("%s ready endpoints=%d，预期 %d", service, seen, count)
			}
			if strings.HasSuffix(service, "-control") && (roles["cluster"] != 1 || roles["unscheduled"] != 1) {
				return nil, fmt.Errorf("%s control 角色不完整: %v", service, roles)
			}
		}
	}
	return expected, nil
}
