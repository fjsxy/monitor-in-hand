package release_test

import (
	"context"
	"crypto/sha256"
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
	product "github.com/fjsxy/monitor-in-hand/internal/release"
	collection "github.com/fjsxy/monitor-in-hand/test/e2e/collection/support"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	authv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	extclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

func TestRelease(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "已发布候选的新装与升级")
}

var ids = map[string][]string{
	"1.19": {"E2E-119-RELEASE-GH87-001", "E2E-119-RELEASE-GH87-002", "E2E-119-RELEASE-GH87-003", "E2E-119-RELEASE-GH87-004", "E2E-119-RELEASE-GH87-005", "E2E-119-RELEASE-GH87-006"},
	"1.28": {"E2E-128-RELEASE-GH87-001", "E2E-128-RELEASE-GH87-002", "E2E-128-RELEASE-GH87-003", "E2E-128-RELEASE-GH87-004", "E2E-128-RELEASE-GH87-005", "E2E-128-RELEASE-GH87-006"},
}

func id(n int) string {
	values := ids[os.Getenv("KUBERNETES_VERSION")]
	if len(values) < n {
		return "unsupported"
	}
	return values[n-1]
}

const ns = "monitor-in-hand-test"
const secondNS = "gh87-metrics"
const opNS = "monitor-in-hand-system"
const releaseName = "monitor-in-hand"
const crdName = "kubestatemetrics.monitoring.fjsxy.github.io"

var smGVR = schema.GroupVersionResource{Group: "monitoring.coreos.com", Version: "v1", Resource: "servicemonitors"}
var agentGVR = schema.GroupVersionResource{Group: "monitoring.coreos.com", Version: "v1alpha1", Resource: "prometheusagents"}
var dc dynamic.Interface
var kc kubernetes.Interface
var ec extclient.Interface
var cc client.Client
var config *rest.Config
var manifest *product.Manifest
var bundle, kubeContext, root string
var names []string
var external map[string]fingerprint

type fingerprint struct {
	UID     string
	Content string
}

var _ = BeforeSuite(func() {
	By("只消费远程已下载的同源 bundle，并核验隔离环境身份")
	minor, run := os.Getenv("KUBERNETES_VERSION"), os.Getenv("TEST_RUN_ID")
	Expect(ids).To(HaveKey(minor))
	Expect(run).To(MatchRegexp(`^[a-z0-9-]+$`))
	bundle = os.Getenv("RELEASE_BUNDLE")
	Expect(bundle).NotTo(BeEmpty())
	Expect(product.VerifyBundle(bundle, os.Getenv("RELEASE_TESTED_COMMIT"))).To(Succeed())
	var err error
	manifest, err = product.ReadManifest(bundle)
	Expect(err).NotTo(HaveOccurred())
	root, err = filepath.Abs("../../..")
	Expect(err).NotTo(HaveOccurred())
	kubeContext = "kind-mih-e2e-" + strings.ReplaceAll(minor, ".", "") + "-" + run
	config, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{CurrentContext: kubeContext}).ClientConfig()
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
	Expect(marker.Data["testRunID"]).To(Equal(run))
	Expect(marker.Data["kubernetesVersion"]).To(Equal(minor))
	_, err = kc.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: secondNS}}, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred())
	AddReportEntry("候选来源与文件摘要", manifest, ReportEntryVisibilityFailureOrVerbose)
})

var _ = Describe("发布契约", Ordered, func() {
	It(id(1)+" 新装缺可选 API 及晚装恢复", func(ctx SpecContext) {
		By("隔离环境准备移除 SM API，然后直接安装远程候选，禁止本地镜像替代")
		Expect(ec.ApiextensionsV1().CustomResourceDefinitions().Delete(ctx, "servicemonitors.monitoring.coreos.com", metav1.DeleteOptions{})).To(Succeed())
		Eventually(func() bool {
			_, e := ec.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, "servicemonitors.monitoring.coreos.com", metav1.GetOptions{})
			return apierrors.IsNotFound(e)
		}, 90*time.Second, time.Second).Should(BeTrue())
		upgrade(ctx)
		checkImage(ctx, manifest.Image, true)
		createCR(ctx, "gh87-off", ns, false)
		createCR(ctx, "gh87-missing", ns, true)
		waitWorkload(ctx, "gh87-off")
		waitWorkload(ctx, "gh87-missing")
		waitCR(ctx, "gh87-missing", func(c *api.KubeStateMetrics) bool {
			return c.Status.ServiceMonitor != nil && c.Status.ServiceMonitor.Reason == "DependencyMissing"
		})
		before := getCR(ctx, "gh87-missing")
		By("安装锁定外部 API 后，不改 CR 即自动生成当前 owner 的 SM")
		data, err := os.ReadFile(filepath.Join(root, "test/fixtures/servicemonitor/monitoring.coreos.com_servicemonitors.yaml"))
		Expect(err).NotTo(HaveOccurred())
		definition := &extv1.CustomResourceDefinition{}
		Expect(yaml.Unmarshal(data, definition)).To(Succeed())
		_, err = ec.ApiextensionsV1().CustomResourceDefinitions().Create(ctx, definition, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		waitMonitor(ctx, "gh87-missing")
		after := getCR(ctx, "gh87-missing")
		Expect(after.UID).To(Equal(before.UID))
		Expect(after.Generation).To(Equal(before.Generation))
		recordOwner(ctx, after)
		deleteCR(ctx, "gh87-off")
		deleteCR(ctx, "gh87-missing")
		By("准备管理员独立 SM，再快照四类配置、原生 Agent 与生成资源")
		_, err = dc.Resource(smGVR).Namespace(ns).Create(ctx, &unstructured.Unstructured{Object: map[string]any{"apiVersion": "monitoring.coreos.com/v1", "kind": "ServiceMonitor", "metadata": map[string]any{"name": "gh87-admin", "namespace": ns}, "spec": map[string]any{"selector": map[string]any{"matchLabels": map[string]any{"no-match": "admin-only"}}, "endpoints": []any{map[string]any{"port": "admin-metrics"}}}}}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		external = snapshotExternal(ctx)
		AddReportEntry("外部对象身份与内容指纹", external, ReportEntryVisibilityFailureOrVerbose)
	}, NodeTimeout(10*time.Minute))
	It(id(2)+" 历史 0.1.0 的独立升级边界", func(ctx SpecContext) {
		By("安装真正历史镜像；旧 schema 明确拒绝新能力消费")
		resetOperator(ctx, manifest.Legacy.Chart)
		checkImage(ctx, manifest.Legacy.Image, false)
		oldSchema, err := ec.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, crdName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		data, err := json.Marshal(oldSchema)
		Expect(err).NotTo(HaveOccurred())
		Expect(product.CheckCapability(data)).NotTo(Succeed())
		createCR(ctx, "gh87-historical", ns, false)
		updateWith(ctx, cc, "gh87-historical", func(c *api.KubeStateMetrics) { c.Finalizers = append(c.Finalizers, "test.monitor-in-hand/gh87") })
		// 只移除本测试添加的外部 finalizer，失败后也不阻塞业务 finalizer 的正常清理。
		DeferCleanup(func() {
			if apierrors.IsNotFound(cc.Get(context.Background(), client.ObjectKey{Name: "gh87-historical"}, &api.KubeStateMetrics{})) {
				return
			}
			updateWith(context.Background(), cc, "gh87-historical", func(c *api.KubeStateMetrics) {
				kept := []string{}
				for _, f := range c.Finalizers {
					if f != "test.monitor-in-hand/gh87" {
						kept = append(kept, f)
					}
				}
				c.Finalizers = kept
			})
		})
		before := getCR(ctx, "gh87-historical")
		By("历史 runtime 无 KSM controller，不能把不存在的旧 workload 算作已保留")
		Consistently(func() bool {
			_, e := kc.AppsV1().Deployments(ns).Get(ctx, "ksm-gh87-historical", metav1.GetOptions{})
			return apierrors.IsNotFound(e)
		}, 5*time.Second, time.Second).Should(BeTrue())
		upgrade(ctx)
		checkImage(ctx, manifest.Image, true)
		waitWorkload(ctx, before.Name)
		after := getCR(ctx, before.Name)
		Expect(after.UID).To(Equal(before.UID))
		Expect(after.Spec.TargetNamespace).To(Equal(before.Spec.TargetNamespace))
		Expect(after.Spec.ServiceMonitor).To(BeNil())
		Expect(after.Finalizers).To(ContainElements(before.Finalizers))
		_, err = dc.Resource(smGVR).Namespace(ns).Get(ctx, "ksm-gh87-historical", metav1.GetOptions{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
		By("同一升级入口可以重复执行，保持原 CR 身份且不自动启用")
		upgrade(ctx)
		Expect(getCR(ctx, before.Name).UID).To(Equal(before.UID))
		updateWith(ctx, cc, before.Name, func(c *api.KubeStateMetrics) {
			kept := []string{}
			for _, f := range c.Finalizers {
				if f != "test.monitor-in-hand/gh87" {
					kept = append(kept, f)
				}
			}
			c.Finalizers = kept
		})
		deleteCR(ctx, before.Name)
		assertExternal(ctx)
	}, NodeTimeout(12*time.Minute))
	It(id(3)+" pre-SM 运行实例升级保持 UID 和归属", func(ctx SpecContext) {
		By("明确安装新发布的功能前对照候选，不称作历史正式 0.1.0")
		resetOperator(ctx, manifest.Previous.Chart)
		checkImage(ctx, manifest.Previous.Image, false)
		createCR(ctx, "gh87-main", ns, false)
		createCR(ctx, "gh87-peer", secondNS, false)
		before := map[string]map[string]string{}
		for _, name := range []string{"gh87-main", "gh87-peer"} {
			waitWorkload(ctx, name)
			before[name] = instanceIdentity(ctx, name)
		}
		AddReportEntry("功能前候选的原身份", before, ReportEntryVisibilityFailureOrVerbose)
		upgrade(ctx)
		checkImage(ctx, manifest.Image, true)
		for _, name := range []string{"gh87-main", "gh87-peer"} {
			waitWorkload(ctx, name)
			waitCR(ctx, name, func(c *api.KubeStateMetrics) bool {
				return c.Status.ServiceMonitor != nil && c.Status.ServiceMonitor.State == "Disabled"
			})
			Expect(instanceIdentity(ctx, name)).To(Equal(before[name]))
			Expect(getCR(ctx, name).Spec.ServiceMonitor).To(BeNil())
		}
		assertExternal(ctx)
	}, NodeTimeout(12*time.Minute))
	It(id(4)+" 受限平台身份只写 CR，Operator 自身权限恢复后配套", func(ctx SpecContext) {
		By("创建只有 exporter 管理和 SM 只读权限的平台身份")
		_, err := kc.CoreV1().ServiceAccounts(ns).Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "gh87-platform", Namespace: ns}}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		_, err = kc.RbacV1().ClusterRoles().Create(ctx, &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "gh87-platform"}, Rules: []rbacv1.PolicyRule{{APIGroups: []string{api.GroupVersion.Group}, Resources: []string{"kubestatemetrics"}, Verbs: []string{"get", "list", "watch", "create", "update", "patch", "delete"}}, {APIGroups: []string{"monitoring.coreos.com"}, Resources: []string{"servicemonitors"}, Verbs: []string{"get", "list", "watch"}}}}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		_, err = kc.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "gh87-platform"}, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "gh87-platform"}, Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: "gh87-platform", Namespace: ns}}}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		platformConfig := impersonate(ns, "gh87-platform")
		platform, err := client.New(platformConfig, client.Options{Scheme: cc.Scheme()})
		Expect(err).NotTo(HaveOccurred())
		platformDynamic, err := dynamic.NewForConfig(platformConfig)
		Expect(err).NotTo(HaveOccurred())
		role, err := kc.RbacV1().ClusterRoles().Get(ctx, "monitor-in-hand-manager-role", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		originalRules := role.DeepCopy().Rules
		restore := func() {
			Eventually(func() error {
				current, e := kc.RbacV1().ClusterRoles().Get(context.Background(), role.Name, metav1.GetOptions{})
				if e != nil {
					return e
				}
				current.Rules = originalRules
				_, e = kc.RbacV1().ClusterRoles().Update(context.Background(), current, metav1.UpdateOptions{})
				return e
			}, 30*time.Second, time.Second).Should(Succeed())
		}
		DeferCleanup(restore)
		role.Rules = nil
		for _, rule := range originalRules {
			if !contains(rule.APIGroups, "monitoring.coreos.com") {
				role.Rules = append(role.Rules, rule)
			}
		}
		_, err = kc.RbacV1().ClusterRoles().Update(ctx, role, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		By("平台真实 CR update 成功，Operator 失权时保留原 workload")
		updateWith(ctx, platform, "gh87-main", enableMonitor)
		waitCR(ctx, "gh87-main", func(c *api.KubeStateMetrics) bool {
			return c.Status.ServiceMonitor != nil && c.Status.ServiceMonitor.Reason == "PermissionDenied"
		})
		waitWorkload(ctx, "gh87-main")
		restore()
		waitMonitor(ctx, "gh87-main")
		updateWith(ctx, platform, "gh87-peer", enableMonitor)
		waitMonitor(ctx, "gh87-peer")
		By("直接 API 验证平台不能写 SM；独立使用 Operator 自身身份验证权限")
		for _, name := range []string{"gh87-main", "gh87-peer"} {
			c := getCR(ctx, name)
			obj := getMonitor(ctx, c)
			_, err = platformDynamic.Resource(smGVR).Namespace(c.Spec.TargetNamespace).Get(ctx, obj.GetName(), metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			_, err = platformDynamic.Resource(smGVR).Namespace(c.Spec.TargetNamespace).Update(ctx, obj, metav1.UpdateOptions{})
			Expect(apierrors.IsForbidden(err)).To(BeTrue())
			err = platformDynamic.Resource(smGVR).Namespace(c.Spec.TargetNamespace).Delete(ctx, obj.GetName(), metav1.DeleteOptions{})
			Expect(apierrors.IsForbidden(err)).To(BeTrue())
			obj.SetName("forbidden-create")
			obj.SetUID("")
			obj.SetResourceVersion("")
			_, err = platformDynamic.Resource(smGVR).Namespace(c.Spec.TargetNamespace).Create(ctx, obj, metav1.CreateOptions{})
			Expect(apierrors.IsForbidden(err)).To(BeTrue())
			recordOwner(ctx, c)
		}
		deployment, err := kc.AppsV1().Deployments(opNS).Get(ctx, "monitor-in-hand-monitor-in-hand", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		operatorClient, err := kubernetes.NewForConfig(impersonate(opNS, deployment.Spec.Template.Spec.ServiceAccountName))
		Expect(err).NotTo(HaveOccurred())
		for _, verb := range []string{"get", "create", "update", "delete"} {
			review, e := operatorClient.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authv1.SelfSubjectAccessReview{Spec: authv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &authv1.ResourceAttributes{Namespace: ns, Group: "monitoring.coreos.com", Version: "v1", Resource: "servicemonitors", Verb: verb}}}, metav1.CreateOptions{})
			Expect(e).NotTo(HaveOccurred())
			Expect(review.Status.Allowed).To(BeTrue(), verb)
		}
		assertExternal(ctx)
	}, NodeTimeout(8*time.Minute))
	It(id(5)+" 已发布产物生成对象经原生 Agent 产生可关联样本", func(ctx SpecContext) {
		By("两命名 namespace 中的配套配置由同一个原生 Agent 选择")
		for _, namespace := range []string{ns, secondNS} {
			current, err := kc.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			if current.Labels == nil {
				current.Labels = map[string]string{}
			}
			current.Labels["gh87-collection"] = "true"
			_, err = kc.CoreV1().Namespaces().Update(ctx, current, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		agent, err := dc.Resource(agentGVR).Namespace(ns).Get(ctx, "fixture", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		agent.SetName("gh87")
		agent.SetUID("")
		agent.SetResourceVersion("")
		agent.SetManagedFields(nil)
		agent.SetGeneration(0)
		agent.SetFinalizers(nil)
		delete(agent.Object, "status")
		spec := agent.Object["spec"].(map[string]any)
		for _, key := range []string{"podMonitorSelector", "probeSelector", "scrapeConfigSelector"} {
			delete(spec, key)
		}
		spec["serviceMonitorSelector"] = map[string]any{"matchLabels": map[string]any{"monitor-mgr.io/user-id": "release-gh87"}}
		spec["serviceMonitorNamespaceSelector"] = map[string]any{"matchLabels": map[string]any{"gh87-collection": "true"}}
		spec["remoteWrite"] = []any{map[string]any{"url": "http://vminsert.monitor-in-hand-test.svc:8480/insert/87/prometheus/api/v1/write"}}
		_, err = dc.Resource(agentGVR).Namespace(ns).Create(ctx, agent, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		assertData(ctx, []string{"gh87-main", "gh87-peer"})
		checkImage(ctx, manifest.Image, true)
		assertExternal(ctx)
	}, NodeTimeout(10*time.Minute))
	It(id(6)+" 删除单实例不影响另一实例，最终清理保持外部资源", func(ctx SpecContext) {
		peerBefore := instanceIdentity(ctx, "gh87-peer")
		deleteCR(ctx, "gh87-main")
		Expect(instanceIdentity(ctx, "gh87-peer")).To(Equal(peerBefore))
		assertData(ctx, []string{"gh87-peer"})
		assertExternal(ctx)
		deleteCR(ctx, "gh87-peer")
		Expect(collection.DeletePrometheusAgent(ctx, config, "gh87", ns)).To(Succeed())
		uninstall(ctx)
		assertExternal(ctx)
	}, NodeTimeout(8*time.Minute))
})

var _ = AfterSuite(func(ctx SpecContext) {
	By("独立清理本轮对象并再次核验外部图，不移除业务 finalizer")
	if dc == nil {
		return
	}
	Expect(collection.DeletePrometheusAgent(ctx, config, "gh87", ns)).To(Succeed())
	for _, name := range names {
		deleteCR(ctx, name)
	}
	uninstall(ctx)
	for _, resource := range []schema.GroupVersionResource{{Group: rbacv1.GroupName, Version: "v1", Resource: "clusterrolebindings"}, {Group: rbacv1.GroupName, Version: "v1", Resource: "clusterroles"}} {
		err := dc.Resource(resource).Delete(ctx, "gh87-platform", metav1.DeleteOptions{})
		Expect(err).To(SatisfyAny(Succeed(), WithTransform(apierrors.IsNotFound, BeTrue())))
	}
	err := kc.CoreV1().ServiceAccounts(ns).Delete(ctx, "gh87-platform", metav1.DeleteOptions{})
	Expect(err).To(SatisfyAny(Succeed(), WithTransform(apierrors.IsNotFound, BeTrue())))
	if external != nil {
		assertExternal(ctx)
	}
	err = dc.Resource(smGVR).Namespace(ns).Delete(ctx, "gh87-admin", metav1.DeleteOptions{})
	Expect(err).To(SatisfyAny(Succeed(), WithTransform(apierrors.IsNotFound, BeTrue())))
	err = kc.CoreV1().Namespaces().Delete(ctx, secondNS, metav1.DeleteOptions{})
	Expect(err).To(SatisfyAny(Succeed(), WithTransform(apierrors.IsNotFound, BeTrue())))
}, NodeTimeout(8*time.Minute))

// 以下 helper 仅通过公开 Kubernetes/Helm/HTTP 边界观察，异步等待均有明确上限。
func command(ctx context.Context, name string, args ...string) error {
	if name == "helm" && os.Getenv("HELM_BIN") != "" {
		name = os.Getenv("HELM_BIN")
	}
	c := exec.CommandContext(ctx, name, args...)
	c.Dir = root
	b, e := c.CombinedOutput()
	if e != nil {
		return fmt.Errorf("%s: %w: %s", name, e, b)
	}
	return nil
}
func upgrade(ctx context.Context) {
	GinkgoHelper()
	Expect(product.Upgrade(ctx, bundle, manifest.SourceCommit, kubeContext, opNS, releaseName, command)).To(Succeed())
	recordDeploymentContract(ctx)
}

// recordDeploymentContract 核验实际 API schema 和生效权限，不能只用发布包本身证明集群已升级。
func recordDeploymentContract(ctx context.Context) {
	GinkgoHelper()
	definition, err := ec.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, crdName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	live, err := json.Marshal(definition)
	Expect(err).NotTo(HaveOccurred())
	Expect(product.CheckCapability(live)).To(Succeed())
	data, err := os.ReadFile(filepath.Join(bundle, manifest.CRD))
	Expect(err).NotTo(HaveOccurred())
	expected := &extv1.CustomResourceDefinition{}
	Expect(yaml.Unmarshal(data, expected)).To(Succeed())
	for _, version := range expected.Spec.Versions {
		found := false
		for _, actual := range definition.Spec.Versions {
			if actual.Name != version.Name {
				continue
			}
			found = true
			Expect(actual.Schema).To(Equal(version.Schema))
			Expect(actual.Served).To(Equal(version.Served))
			Expect(actual.Storage).To(Equal(version.Storage))
			Expect(actual.Subresources).To(Equal(version.Subresources))
		}
		Expect(found).To(BeTrue(), version.Name)
	}
	role, err := kc.RbacV1().ClusterRoles().Get(ctx, "monitor-in-hand-manager-role", metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	data, err = os.ReadFile(filepath.Join(bundle, manifest.RBAC))
	Expect(err).NotTo(HaveOccurred())
	expectedRole := &rbacv1.ClusterRole{}
	Expect(yaml.Unmarshal(data, expectedRole)).To(Succeed())
	Expect(role.Rules).To(Equal(expectedRole.Rules))
	schemaBytes, err := json.Marshal(definition.Spec.Versions)
	Expect(err).NotTo(HaveOccurred())
	rbacBytes, err := json.Marshal(role.Rules)
	Expect(err).NotTo(HaveOccurred())
	AddReportEntry("实际部署 schema 与 RBAC", map[string]string{"crdUID": string(definition.UID), "schemaSHA256": fmt.Sprintf("%x", sha256.Sum256(schemaBytes)), "rbacUID": string(role.UID), "rulesSHA256": fmt.Sprintf("%x", sha256.Sum256(rbacBytes))}, ReportEntryVisibilityFailureOrVerbose)
}

func uninstall(ctx context.Context) {
	GinkgoHelper()
	Expect(command(ctx, "helm", "uninstall", releaseName, "--kube-context", kubeContext, "--namespace", opNS, "--wait", "--timeout", "120s", "--ignore-not-found")).To(Succeed())
}
func resetOperator(ctx context.Context, chart string) {
	GinkgoHelper()
	uninstall(ctx)
	Expect(ec.ApiextensionsV1().CustomResourceDefinitions().Delete(ctx, crdName, metav1.DeleteOptions{})).To(Succeed())
	Eventually(func() bool {
		_, e := ec.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, crdName, metav1.GetOptions{})
		return apierrors.IsNotFound(e)
	}, 90*time.Second, time.Second).Should(BeTrue())
	Expect(command(ctx, "helm", "upgrade", "--install", releaseName, filepath.Join(bundle, chart), "--kube-context", kubeContext, "--namespace", opNS, "--create-namespace", "--wait", "--timeout", "180s")).To(Succeed())
}
func createCR(ctx context.Context, name, namespace string, enabled bool) {
	GinkgoHelper()
	c := &api.KubeStateMetrics{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: api.KubeStateMetricsSpec{TargetNamespace: namespace}}
	if enabled {
		enableMonitor(c)
	}
	Expect(cc.Create(ctx, c)).To(Succeed())
	names = append(names, name)
}
func enableMonitor(c *api.KubeStateMetrics) {
	c.Spec.ServiceMonitor = &api.KubeStateMetricsServiceMonitor{Enabled: true, Labels: map[string]string{"monitor-mgr.io/user-id": "release-gh87"}, Interval: "5s", ScrapeTimeout: "3s"}
}
func getCR(ctx context.Context, name string) *api.KubeStateMetrics {
	GinkgoHelper()
	c := &api.KubeStateMetrics{}
	Expect(cc.Get(ctx, client.ObjectKey{Name: name}, c)).To(Succeed())
	return c
}
func waitCR(ctx context.Context, name string, predicate func(*api.KubeStateMetrics) bool) {
	GinkgoHelper()
	Eventually(func() bool {
		c := &api.KubeStateMetrics{}
		return cc.Get(ctx, client.ObjectKey{Name: name}, c) == nil && predicate(c)
	}, 180*time.Second, time.Second).Should(BeTrue(), name)
}
func waitWorkload(ctx context.Context, name string) {
	GinkgoHelper()
	waitCR(ctx, name, func(c *api.KubeStateMetrics) bool {
		return c.Status.ObservedGeneration == c.Generation && c.Status.DesiredPods > 0 && c.Status.AvailablePods >= c.Status.DesiredPods
	})
}
func waitMonitor(ctx context.Context, name string) {
	GinkgoHelper()
	waitCR(ctx, name, func(c *api.KubeStateMetrics) bool {
		return c.Status.ServiceMonitor != nil && c.Status.ServiceMonitor.ObservedGeneration == c.Generation && c.Status.ServiceMonitor.State == "Ready"
	})
}
func getMonitor(ctx context.Context, c *api.KubeStateMetrics) *unstructured.Unstructured {
	GinkgoHelper()
	Expect(c.Status.ServiceMonitor).NotTo(BeNil())
	Expect(c.Status.ServiceMonitor.Resource).NotTo(BeNil())
	o, e := dc.Resource(smGVR).Namespace(c.Spec.TargetNamespace).Get(ctx, c.Status.ServiceMonitor.Resource.Name, metav1.GetOptions{})
	Expect(e).NotTo(HaveOccurred())
	return o
}
func recordOwner(ctx context.Context, c *api.KubeStateMetrics) {
	GinkgoHelper()
	obj := getMonitor(ctx, c)
	Expect(obj.GetNamespace()).To(Equal(c.Spec.TargetNamespace))
	Expect(string(obj.GetUID())).To(Equal(c.Status.ServiceMonitor.Resource.UID))
	owner := metav1.GetControllerOf(obj)
	Expect(owner).NotTo(BeNil())
	Expect(owner.UID).To(Equal(c.UID))
	AddReportEntry("当前 CR/配套对象身份", map[string]any{"crName": c.Name, "crUID": c.UID, "monitorNamespace": obj.GetNamespace(), "monitorName": obj.GetName(), "monitorUID": obj.GetUID(), "owner": owner}, ReportEntryVisibilityFailureOrVerbose)
}
func deleteCR(ctx context.Context, name string) {
	GinkgoHelper()
	c := &api.KubeStateMetrics{}
	e := cc.Get(ctx, client.ObjectKey{Name: name}, c)
	if apierrors.IsNotFound(e) {
		return
	}
	Expect(e).NotTo(HaveOccurred())
	Expect(cc.Delete(ctx, c)).To(Succeed())
	Eventually(func() bool {
		return apierrors.IsNotFound(cc.Get(ctx, client.ObjectKey{Name: name}, &api.KubeStateMetrics{}))
	}, 150*time.Second, time.Second).Should(BeTrue(), name)
	_, e = dc.Resource(smGVR).Namespace(c.Spec.TargetNamespace).Get(ctx, "ksm-"+name, metav1.GetOptions{})
	Expect(apierrors.IsNotFound(e)).To(BeTrue())
	for _, resource := range []string{"deployments", "statefulsets", "daemonsets"} {
		_, e = dc.Resource(schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: resource}).Namespace(c.Spec.TargetNamespace).Get(ctx, "ksm-"+name, metav1.GetOptions{})
		Expect(apierrors.IsNotFound(e)).To(BeTrue())
	}
}
func updateWith(ctx context.Context, c client.Client, name string, change func(*api.KubeStateMetrics)) {
	GinkgoHelper()
	Eventually(func() error {
		obj := &api.KubeStateMetrics{}
		if e := c.Get(ctx, client.ObjectKey{Name: name}, obj); e != nil {
			return e
		}
		change(obj)
		return c.Update(ctx, obj)
	}, 30*time.Second, time.Second).Should(Succeed())
}
func instanceIdentity(ctx context.Context, name string) map[string]string {
	GinkgoHelper()
	c := getCR(ctx, name)
	deployment, e := kc.AppsV1().Deployments(c.Spec.TargetNamespace).Get(ctx, "ksm-"+name, metav1.GetOptions{})
	Expect(e).NotTo(HaveOccurred())
	service, e := kc.CoreV1().Services(c.Spec.TargetNamespace).Get(ctx, "ksm-"+name, metav1.GetOptions{})
	Expect(e).NotTo(HaveOccurred())
	Expect(metav1.GetControllerOf(deployment).UID).To(Equal(c.UID))
	Expect(metav1.GetControllerOf(service).UID).To(Equal(c.UID))
	return map[string]string{"crUID": string(c.UID), "namespace": c.Spec.TargetNamespace, "deploymentUID": string(deployment.UID), "serviceUID": string(service.UID), "finalizers": strings.Join(c.Finalizers, ",")}
}
func impersonate(namespace, name string) *rest.Config {
	c := rest.CopyConfig(config)
	c.Impersonate = rest.ImpersonationConfig{UserName: "system:serviceaccount:" + namespace + ":" + name, Groups: []string{"system:serviceaccounts", "system:serviceaccounts:" + namespace, "system:authenticated"}}
	return c
}
func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func checkImage(ctx context.Context, image product.Image, pinned bool) {
	GinkgoHelper()
	var pod *corev1.Pod
	var arch string
	Eventually(func() bool {
		d, e := kc.AppsV1().Deployments(opNS).Get(ctx, "monitor-in-hand-monitor-in-hand", metav1.GetOptions{})
		if e != nil || d.Status.ObservedGeneration < d.Generation || d.Status.AvailableReplicas != 1 || d.Status.UpdatedReplicas != 1 || d.Status.Replicas != 1 {
			return false
		}
		if pinned && d.Spec.Template.Spec.Containers[0].Image != image.Repository+"@"+image.IndexDigest {
			return false
		}
		pods, e := kc.CoreV1().Pods(opNS).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/instance=monitor-in-hand"})
		if e != nil {
			return false
		}
		for i := range pods.Items {
			p := &pods.Items[i]
			if p.DeletionTimestamp != nil {
				continue
			}
			for _, s := range p.Status.ContainerStatuses {
				if s.Name == "manager" && s.Ready {
					node, e := kc.CoreV1().Nodes().Get(ctx, p.Spec.NodeName, metav1.GetOptions{})
					if e != nil {
						return false
					}
					arch = node.Status.NodeInfo.Architecture
					want := image.Platforms["linux/"+arch]
					if want == "" || (!strings.HasSuffix(s.ImageID, want) && !strings.HasSuffix(s.ImageID, image.IndexDigest)) {
						return false
					}
					pod = p
					return true
				}
			}
		}
		return false
	}, 180*time.Second, time.Second).Should(BeTrue(), image.Tag)
	// containerd 在某些版本报告 index imageID；进一步比较实际 runtime config 与锁定平台 manifest 的 config digest。
	imageID := pod.Status.ContainerStatuses[0].ImageID
	runtimeData, err := exec.CommandContext(ctx, "docker", "exec", pod.Spec.NodeName, "crictl", "inspecti", imageID).CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), string(runtimeData))
	var runtimeImage struct {
		Status struct {
			ID string `json:"id"`
		} `json:"status"`
		Info struct {
			ImageSpec struct {
				Architecture string `json:"architecture"`
				OS           string `json:"os"`
				Config       struct {
					Labels map[string]string `json:"Labels"`
				} `json:"config"`
			} `json:"imageSpec"`
		} `json:"info"`
	}
	Expect(json.Unmarshal(runtimeData, &runtimeImage)).To(Succeed())
	Expect(runtimeImage.Info.ImageSpec.Architecture).To(Equal(arch))
	Expect(runtimeImage.Info.ImageSpec.OS).To(Equal("linux"))
	platformData, err := exec.CommandContext(ctx, "docker", "buildx", "imagetools", "inspect", image.Repository+"@"+image.Platforms["linux/"+arch], "--raw").CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), string(platformData))
	var platformManifest struct {
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
	}
	Expect(json.Unmarshal(platformData, &platformManifest)).To(Succeed())
	Expect(platformManifest.Config.Digest).NotTo(BeEmpty())
	Expect(runtimeImage.Status.ID).To(Equal(platformManifest.Config.Digest))
	if image.SourceCommit != "" {
		Expect(runtimeImage.Info.ImageSpec.Config.Labels["org.opencontainers.image.revision"]).To(Equal(image.SourceCommit))
	}
	AddReportEntry("运行时与平台 manifest 的 config digest", map[string]string{"runtimeConfigDigest": runtimeImage.Status.ID, "platformConfigDigest": platformManifest.Config.Digest}, ReportEntryVisibilityFailureOrVerbose)
	AddReportEntry("实际运行镜像", map[string]any{"tag": image.Tag, "sourceCommit": image.SourceCommit, "nodeArchitecture": arch, "podUID": pod.UID, "podImage": pod.Spec.Containers[0].Image, "imageID": pod.Status.ContainerStatuses[0].ImageID, "indexDigest": image.IndexDigest, "platformDigest": image.Platforms["linux/"+arch]}, ReportEntryVisibilityFailureOrVerbose)
}

// snapshotExternal 只输出 UID 与内容摘要，避免把外部 Secret 或配置正文放入公开测试日志。
func snapshotExternal(ctx context.Context) map[string]fingerprint {
	GinkgoHelper()
	result := map[string]fingerprint{}
	for _, gvr := range []schema.GroupVersionResource{agentGVR, smGVR, {Group: "monitoring.coreos.com", Version: "v1", Resource: "podmonitors"}, {Group: "monitoring.coreos.com", Version: "v1", Resource: "probes"}, {Group: "monitoring.coreos.com", Version: "v1alpha1", Resource: "scrapeconfigs"}, {Group: "apps", Version: "v1", Resource: "statefulsets"}, {Version: "v1", Resource: "pods"}, {Version: "v1", Resource: "secrets"}, {Version: "v1", Resource: "persistentvolumeclaims"}, {Version: "v1", Resource: "services"}} {
		list, e := dc.Resource(gvr).Namespace(ns).List(ctx, metav1.ListOptions{})
		Expect(e).NotTo(HaveOccurred())
		for _, o := range list.Items {
			if strings.HasPrefix(o.GetName(), "gh87") && o.GetName() != "gh87-admin" {
				continue
			}
			if gvr.Group == "" || gvr.Group == "apps" {
				if !strings.Contains(o.GetName(), "fixture") && o.GetName() != "prometheus-operated" {
					continue
				}
			}
			data, e := json.Marshal(map[string]any{"spec": o.Object["spec"], "data": o.Object["data"], "owners": o.GetOwnerReferences()})
			Expect(e).NotTo(HaveOccurred())
			key := gvr.Group + "|" + gvr.Version + "|" + gvr.Resource + "|" + o.GetName()
			result[key] = fingerprint{UID: string(o.GetUID()), Content: fmt.Sprintf("%x", sha256.Sum256(data))}
		}
	}
	return result
}
func assertExternal(ctx context.Context) {
	GinkgoHelper()
	actual := snapshotExternal(ctx)
	for key, want := range external {
		Expect(actual).To(HaveKeyWithValue(key, want), key)
	}
}

func forward(ctx context.Context, target string, port int) (string, func()) {
	GinkgoHelper()
	l, e := net.Listen("tcp", "127.0.0.1:0")
	Expect(e).NotTo(HaveOccurred())
	local := l.Addr().(*net.TCPAddr).Port
	Expect(l.Close()).To(Succeed())
	c := exec.CommandContext(ctx, "kubectl", "--context", kubeContext, "--namespace", ns, "port-forward", target, fmt.Sprintf("%d:%d", local, port))
	Expect(c.Start()).To(Succeed())
	stop := func() { _ = c.Process.Kill(); _ = c.Wait() }
	DeferCleanup(stop)
	Eventually(func() error {
		conn, e := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", local), time.Second)
		if e == nil {
			conn.Close()
		}
		return e
	}, 30*time.Second, time.Second).Should(Succeed())
	return fmt.Sprintf("http://127.0.0.1:%d", local), stop
}
func getHTTP(ctx context.Context, address string) ([]byte, error) {
	r, e := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if e != nil {
		return nil, e
	}
	response, e := (&http.Client{Timeout: 5 * time.Second}).Do(r)
	if e != nil {
		return nil, e
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", response.StatusCode)
	}
	return io.ReadAll(response.Body)
}

func assertData(ctx context.Context, expectedNames []string) {
	GinkgoHelper()
	By("比对必要 Service 的真实 target，并查询同环境接收端的可关联样本")
	expected := map[string]bool{}
	for _, name := range expectedNames {
		c := getCR(ctx, name)
		recordOwner(ctx, c)
		service := "ksm-" + name
		Eventually(func() bool {
			ep, e := kc.CoreV1().Endpoints(c.Spec.TargetNamespace).Get(ctx, service, metav1.GetOptions{})
			if e != nil {
				return false
			}
			current := map[string]bool{}
			count := 0
			for _, subset := range ep.Subsets {
				for _, a := range subset.Addresses {
					current[c.Spec.TargetNamespace+"|"+service+"|"+a.IP+":8080"] = true
					count++
				}
			}
			if count != 1 {
				return false
			}
			for key := range current {
				expected[key] = true
			}
			return true
		}, 120*time.Second, time.Second).Should(BeTrue())
	}
	Eventually(func() bool {
		p, e := kc.CoreV1().Pods(ns).Get(ctx, "prom-agent-gh87-0", metav1.GetOptions{})
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
	base, stop := forward(ctx, "pod/prom-agent-gh87-0", 9090)
	defer stop()
	Eventually(func() error {
		b, e := getHTTP(ctx, base+"/api/v1/targets")
		if e != nil {
			return e
		}
		var v struct {
			Data struct {
				Targets []struct {
					Health     string            `json:"health"`
					URL        string            `json:"scrapeUrl"`
					Discovered map[string]string `json:"discoveredLabels"`
				} `json:"activeTargets"`
			} `json:"data"`
		}
		if e = json.Unmarshal(b, &v); e != nil {
			return e
		}
		actual := map[string]bool{}
		for _, t := range v.Data.Targets {
			if t.Health != "up" {
				return fmt.Errorf("target 未 up")
			}
			u, e := url.Parse(t.URL)
			if e != nil {
				return e
			}
			if u.Path != "/metrics" {
				return fmt.Errorf("错误 metrics 路径")
			}
			actual[t.Discovered["__meta_kubernetes_namespace"]+"|"+t.Discovered["__meta_kubernetes_service_name"]+"|"+u.Host] = true
		}
		if !reflect.DeepEqual(actual, expected) {
			return fmt.Errorf("actual=%v expected=%v", actual, expected)
		}
		AddReportEntry("真实 targets", string(b), ReportEntryVisibilityFailureOrVerbose)
		return nil
	}, 180*time.Second, 2*time.Second).Should(Succeed())
	queryBase, queryStop := forward(ctx, "service/vmselect", 8481)
	defer queryStop()
	query := url.QueryEscape(`count by (namespace,service,instance) (kube_pod_info{prometheus="monitor-in-hand-test/gh87"})`)
	Eventually(func() error {
		b, e := getHTTP(ctx, queryBase+"/select/87/prometheus/api/v1/query?query="+query)
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
		if e = json.Unmarshal(b, &v); e != nil {
			return e
		}
		if v.Status != "success" {
			return fmt.Errorf("query 未成功")
		}
		seen := map[string]bool{}
		for _, sample := range v.Data.Result {
			m := sample.Metric
			seen[m["namespace"]+"|"+m["service"]+"|"+m["instance"]] = true
		}
		for key := range expected {
			if !seen[key] {
				return fmt.Errorf("缺少必要端点的真实样本: %s", key)
			}
		}
		AddReportEntry("真实 remote_write 样本", string(b), ReportEntryVisibilityFailureOrVerbose)
		return nil
	}, 180*time.Second, 2*time.Second).Should(Succeed())
}
