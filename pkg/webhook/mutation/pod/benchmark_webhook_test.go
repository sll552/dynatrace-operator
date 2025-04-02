package pod

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Dynatrace/dynatrace-operator/pkg/api/scheme"
	"github.com/Dynatrace/dynatrace-operator/pkg/api/shared/communication"
	"github.com/Dynatrace/dynatrace-operator/pkg/api/status"
	"github.com/Dynatrace/dynatrace-operator/pkg/api/v1beta4/dynakube"
	"github.com/Dynatrace/dynatrace-operator/pkg/api/v1beta4/dynakube/oneagent"
	"github.com/Dynatrace/dynatrace-operator/pkg/consts"
	"github.com/Dynatrace/dynatrace-operator/pkg/webhook/mutation/pod/common/events"
	v1 "github.com/Dynatrace/dynatrace-operator/pkg/webhook/mutation/pod/v1"
	v2 "github.com/Dynatrace/dynatrace-operator/pkg/webhook/mutation/pod/v2"
	"github.com/spf13/afero"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	"sigs.k8s.io/controller-runtime/tools/setup-envtest/env"
	"sigs.k8s.io/controller-runtime/tools/setup-envtest/remote"
	"sigs.k8s.io/controller-runtime/tools/setup-envtest/store"
	"sigs.k8s.io/controller-runtime/tools/setup-envtest/versions"
	"sigs.k8s.io/controller-runtime/tools/setup-envtest/workflows"
)

const (
	dynatraceNsName = "dynatrace"
	testNsPrefix    = "ns-"
	testAPIUrl      = "example.com/testingest"
)

var (
	nsCnt           = 5 * 1000
	dkCnt           = 20
	requestCnt      = 2 * 1000
	testEnv         *envtest.Environment
	testClient      *kubernetes.Clientset
	dkClient        client.Client
	testLabelValues []string = make([]string, dkCnt)
)

func TestMain(m *testing.M) {
	setupTests()
	ret := m.Run()
	tearDownTests()
	os.Exit(ret)
}

func tearDownTests() {
	if testEnv != nil {
		testEnv.Stop()
		log.Info("Stopped testEnv")
	}
}

func setupTests() {
	log.Info("Starting setup of testEnv")
	testEnv = setupEnvTest()
	testEnv.Start()
	testClient = kubernetes.NewForConfigOrDie(testEnv.Config)
	createDkClient()
	createTestLabelValues()
	createNamespaces()
	createDynakubes()
	log.Info("Finished setup of testEnv")
}

func createDkClient() {
	cl, err := client.New(testEnv.Config, client.Options{Scheme: testEnv.Scheme})
	if err != nil {
		log.Error(err, "Could not create client")
		panic(err)
	}
	dkClient = cl
}

func createTestLabelValues() {
	for i := 0; i < dkCnt; i++ {
		testLabelValues[i] = "selector-" + strconv.Itoa(dkCnt)
	}
}

func createNamespaces() {
	var wg sync.WaitGroup
	wg.Add(nsCnt)
	log.Info("Adding namespaces, this may take a while...")

	// Create a new config with less clientside throttling
	var clientConfig = rest.CopyConfig(testEnv.Config)
	clientConfig.QPS = 50
	clientConfig.Burst = 100
	var client = kubernetes.NewForConfigOrDie(clientConfig)

	for cnt := 0; cnt < nsCnt; cnt++ {
		go func() {
			defer wg.Done()
			nsName := testNsPrefix + strconv.Itoa(cnt)
			_, err := client.CoreV1().Namespaces().Create(context.TODO(), &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: nsName,
					Labels: map[string]string{
						"testLabel":  testLabelValues[cnt%dkCnt],
						"someLabel1": "asdf",
						"someLabel2": "asdf",
						"someLabel3": "asdf",
						"someLabel4": "asdf",
						"someLabel5": "asdf",
						"true":       "true",
						"false":      "false",
					},
				},
			}, metav1.CreateOptions{})
			if err != nil {
				log.Error(err, "Error during setup of namespace", "namespace", nsName)
				return
			}
			// Create oneagent/codemodule config
			_, err = client.CoreV1().Secrets(nsName).Create(context.TODO(), &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name: consts.AgentInitSecretName,
				},
				Type: corev1.SecretTypeOpaque,
				StringData: map[string]string{
					consts.AgentInitSecretConfigField:   "{}",
					consts.ActiveGateCAsInitSecretField: "",
					consts.TrustedCAsInitSecretField:    "",
					"proxy":                             "",
				},
			}, metav1.CreateOptions{})
			if err != nil {
				log.Error(err, "Error during setup of namespace", "namespace", nsName)
				return
			}
			// create metadata enrichment config
			_, err = client.CoreV1().Secrets(nsName).Create(context.TODO(), &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name: consts.EnrichmentEndpointSecretName,
				},
				Type: corev1.SecretTypeOpaque,
				StringData: map[string]string{
					"endpoint.properties": "",
				},
			}, metav1.CreateOptions{})
			if err != nil {
				log.Error(err, "Error during setup of namespace", "namespace", nsName)
				return
			}
		}()
	}
	wg.Wait()
}

func setupEnvTest() *envtest.Environment {
	envTestDir, err := store.DefaultStoreDir()
	if err != nil {
		panic(err)
	}
	envTest := &env.Env{
		FS:  afero.Afero{Fs: afero.NewOsFs()},
		Out: os.Stdout,
		Client: &remote.HTTPClient{
			IndexURL: remote.DefaultIndexURL,
		},
		Platform: versions.PlatformItem{
			Platform: versions.Platform{
				OS:   runtime.GOOS,
				Arch: runtime.GOARCH,
			},
		},
		Version: versions.AnyVersion,
		Store:   store.NewAt(envTestDir),
	}
	envTest.CheckCoherence()
	workflows.Use{}.Do(envTest)
	versionDir := envTest.Platform.Platform.BaseName(*envTest.Version.AsConcrete())
	return &envtest.Environment{
		BinaryAssetsDirectory: filepath.Join(envTestDir, "k8s", versionDir),
		// Maybe this can be improved :)
		CRDDirectoryPaths: []string{filepath.Join("..", "..", "..", "..", "config", "crd", "bases")},
		Scheme:            scheme.Scheme,
	}
}

type TestDynakube struct {
	*dynakube.DynaKube
}

func createTestDynakube(name string) (dk *TestDynakube) {
	dk = &TestDynakube{
		DynaKube: &dynakube.DynaKube{
			ObjectMeta: metav1.ObjectMeta{
				Name:        name,
				Namespace:   dynatraceNsName,
				Annotations: map[string]string{},
			},
			Spec: dynakube.DynaKubeSpec{
				APIURL: testAPIUrl,
				OneAgent: oneagent.Spec{
					CloudNativeFullStack: &oneagent.CloudNativeFullStackSpec{},
				},
				MetadataEnrichment: dynakube.MetadataEnrichment{},
			},
		},
	}
	return
}

func (dk *TestDynakube) WithOneAgentNamespaceSelector(selector metav1.LabelSelector) *TestDynakube {
	dk.Spec.OneAgent.CloudNativeFullStack.NamespaceSelector = selector
	return dk
}

func (dk *TestDynakube) WithMetadataNamespaceSelector(selector metav1.LabelSelector) *TestDynakube {
	dk.Spec.MetadataEnrichment.Enabled = ptr.To(true)
	dk.Spec.MetadataEnrichment.NamespaceSelector = selector
	return dk
}

func createDynakubes() {
	_, err := testClient.CoreV1().Namespaces().Create(context.TODO(), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: dynatraceNsName,
		},
	}, metav1.CreateOptions{})
	if err != nil {
		log.Error(err, "Error during setup of namespace", "namespace", dynatraceNsName)
	}

	for i := 0; i < dkCnt; i++ {
		dk := createTestDynakube("testdk-" + strconv.Itoa(i)).WithOneAgentNamespaceSelector(metav1.LabelSelector{
			MatchLabels: map[string]string{
				"testLabel":  testLabelValues[i%dkCnt],
				"someLabel5": "asdf",
				"false":      "false",
			},
		}).WithMetadataNamespaceSelector(metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{
				{
					Key:      "testLabel",
					Operator: metav1.LabelSelectorOpIn,
					Values:   testLabelValues,
				},
				{
					Key:      "someLabel3",
					Operator: metav1.LabelSelectorOpExists,
				},
				{
					Key:      "asdfasdf",
					Operator: metav1.LabelSelectorOpDoesNotExist,
				},
			},
		})

		err = dkClient.Create(context.TODO(), dk.DynaKube)
		if err != nil {
			log.Error(err, "Error creating Dynakube", "name", dk.Name)
		}

		dk.Status = dynakube.DynaKubeStatus{
			Phase:                 status.Running,
			KubeSystemUUID:        testClusterID,
			KubernetesClusterMEID: testClusterID,
			KubernetesClusterName: testClusterID,
			OneAgent: oneagent.Status{
				ConnectionInfoStatus: oneagent.ConnectionInfoStatus{
					ConnectionInfo: communication.ConnectionInfo{
						TenantUUID: testClusterID,
						Endpoints:  testAPIUrl,
					},
					CommunicationHosts: []oneagent.CommunicationHostStatus{
						{
							Protocol: "https",
							Host:     testAPIUrl,
							Port:     12345,
						},
					},
				},
				VersionStatus: status.VersionStatus{
					Version: "123",
					Source:  "tenant-registry",
					ImageID: "asdf/asdf",
					Type:    "immutable",
				},
			},
			CodeModules: oneagent.CodeModulesStatus{
				VersionStatus: status.VersionStatus{
					Version: "123",
					Source:  "tenant-registry",
					ImageID: "asdf/asdf",
					Type:    "immutable",
				},
			},
		}
		err = dkClient.Status().Update(context.TODO(), dk.DynaKube)
		if err != nil {
			log.Error(err, "Error setting status of Dynakube", "name", dk.Name)
		}
	}
}

type NoOpRecorder struct {
}

// AnnotatedEventf implements record.EventRecorder.
func (n *NoOpRecorder) AnnotatedEventf(object k8sruntime.Object, annotations map[string]string, eventtype string, reason string, messageFmt string, args ...interface{}) {
	return
}

// Event implements record.EventRecorder.
func (n *NoOpRecorder) Event(object k8sruntime.Object, eventtype string, reason string, message string) {
	return
}

// Eventf implements record.EventRecorder.
func (n *NoOpRecorder) Eventf(object k8sruntime.Object, eventtype string, reason string, messageFmt string, args ...interface{}) {
	return
}

func createWebhook() *webhook {
	var recorder = events.NewRecorder(&NoOpRecorder{})
	var clientConfig = rest.CopyConfig(testEnv.Config)
	clientConfig.QPS = 50
	clientConfig.Burst = 200
	var client, _ = client.New(clientConfig, client.Options{Scheme: testEnv.Scheme})

	return &webhook{
		v1:               v1.NewInjector(client, client, client, recorder, testClusterID, "someregistry/webhook", dynatraceNsName),
		v2:               v2.NewInjector(client, client, client, recorder),
		recorder:         recorder,
		decoder:          admission.NewDecoder(testEnv.Scheme),
		apiReader:        client,
		webhookNamespace: dynatraceNsName,
		deployedViaOLM:   false,
	}
}

func TestEnvironment(t *testing.T) {
	var resources, err = testClient.DiscoveryClient.ServerPreferredResources()
	if err != nil {
		t.Errorf("error getting resources: %v", err)
	}
	for _, resourceLists := range resources {
		var groupversion = resourceLists.GroupVersion
		for _, v := range resourceLists.APIResources {
			t.Logf("group: %v kind: %v", groupversion, v.Kind)
		}
	}

	dkList := &dynakube.DynaKubeList{}
	err = dkClient.List(context.TODO(), dkList, &client.ListOptions{
		Namespace: dynatraceNsName,
	})
	if err != nil {
		t.Error(err, "Error while listing dynakubes")
	}

	for _, dk := range dkList.Items {
		t.Logf("Got Dynakube: %v in namespace %v: api: %v", dk.Name, dk.Namespace, dk.ApiUrl())
		ten, err := dk.TenantUUID()
		t.Logf("tenant: %v, err: %v", ten, err)
		t.Logf("status: %v", dk.Status)
	}
}

func TestBenchmarkWebhookHandle(t *testing.T) {
	var webhook = createWebhook()
	var testPod = getTestPod()
	var testPodName = testPod.Name

	for i := 0; i < requestCnt; i++ {
		t.Run("", func(t *testing.T) {
			t.Parallel()
			testPod.ObjectMeta.Name = testPodName + strconv.Itoa(i)
			ns := testNsPrefix + strconv.Itoa(i%nsCnt)
			testPod.ObjectMeta.Namespace = ns
			t.Logf("testing namespace %v", ns)
			req := createTestAdmissionRequest(testPod)
			req.Namespace = ns

			resp := webhook.Handle(context.TODO(), *req)
			if reflect.DeepEqual(resp, admission.Patched("")) {
				t.Fail()
			}
		})
	}
}

func BenchmarkWebhookHandle(b *testing.B) {
	var webhook = createWebhook()
	var testPod = getTestPod()
	var testPodName = testPod.Name
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		testPod.ObjectMeta.Name = testPodName + strconv.Itoa(i)
		ns := testNsPrefix + strconv.Itoa(i%nsCnt)
		testPod.ObjectMeta.Namespace = ns
		req := createTestAdmissionRequest(testPod)
		req.Namespace = ns
		b.StartTimer()
		resp := webhook.Handle(context.TODO(), *req)
		b.StopTimer()
		if reflect.DeepEqual(resp, admission.Patched("")) {
			b.Error("response invalid")
		}
		b.StartTimer()
	}
}

func BenchmarkWebhookHandleParallel(b *testing.B) {
	var webhook = createWebhook()
	var cnt = atomic.Int64{}
	b.ResetTimer()

	b.RunParallel(func(p *testing.PB) {
		for p.Next() {
			var testPod = getTestPod()
			var testPodName = testPod.Name
			var i = cnt.Add(1)
			var ns = testNsPrefix + strconv.FormatInt(i%int64(nsCnt), 10)

			testPod.ObjectMeta.Name = testPodName + strconv.FormatInt(i, 10)
			testPod.ObjectMeta.Namespace = ns
			req := createTestAdmissionRequest(testPod)
			req.Namespace = ns

			resp := webhook.Handle(context.TODO(), *req)
			if reflect.DeepEqual(resp, admission.Patched("")) {
				b.Error("response invalid")
			}
		}
	})
}
