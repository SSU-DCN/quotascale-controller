package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"
	"strings"
	"time"

	"github.com/SSU-DCN/quotascale-controller/internal/nodescaling"
	"github.com/SSU-DCN/quotascale-controller/internal/quota"
	"github.com/SSU-DCN/quotascale-controller/internal/resize"
	"github.com/SSU-DCN/quotascale-controller/pkg/kubeconfig"
	"github.com/SSU-DCN/quotascale-controller/pkg/logging"
	ichp "github.com/SSU-DCN/quotascale-controller/pkg/scalerclient/client/clientset/versioned"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
)

func main() {
	enableNodeScaling := flag.Bool("enable-node-scaling", false, "Enable GitOps-based node scaling integration.")
	quotaCheckInterval := flag.Duration("quota-check-interval", time.Minute, "Interval for polling ResourceQuota usage for every QuotaAutoscaler.")
	quotaUpdateInterval := flag.Duration("quota-update-interval", time.Minute, "Minimum interval between resize requests for the same namespace.")
	nodeScaleInDelay := flag.Duration("node-scale-in-delay", 5*time.Minute, "How long scale-in eligibility must remain true before the node scaling controller triggers scale-in.")
	enableNodeScaleInForce := flag.Bool("enable-node-scale-in-force", true, "Enable forced completion of scale-in waiters after the configured force delay.")
	nodeScaleInForceDelay := flag.Duration("node-scale-in-force-delay", 5*time.Minute, "How long a reserved scale-in node may keep blocking pods before the controller forces scale-in anyway.")
	nodeScaleInExemptNamespaces := flag.String("node-scale-in-exempt-namespaces", "kube-system", "Comma-separated namespaces whose pods should not block node scale-in.")
	nodeScaleInExemptPodKey := flag.String("node-scale-in-exempt-pod-key", "quotascale.dcn.ssu.ac.kr/scale-in-exempt", "Label or annotation key marking pods that should not block node scale-in.")
	nodeScaleInExemptPodValue := flag.String("node-scale-in-exempt-pod-value", "true", "Label or annotation value marking pods that should not block node scale-in.")
	nodeScalingMaxNodes := flag.Int("node-scaling-max-nodes", 3, "Maximum MachineDeployment replica count allowed for node scaling.")
	nodeScalingPreparedSpares := flag.Int("node-scaling-prepared-spares", 1, "How many warm spare scaling nodes should remain prepared by default.")
	nodeScalingActivatedSpares := flag.Int("node-scaling-activated-spares", 1, "How many prepared spare nodes should be activated (untainted) per scale-out reconciliation.")
	nodeScalingRepoURL := flag.String("node-scaling-repo-url", "", "Git repository URL for node scaling MachineDeployment manifests. Overrides GITEA_REPO_URL when set.")
	nodeScalingRepoBranch := flag.String("node-scaling-repo-branch", "", "Git branch for node scaling manifests. Overrides GITEA_REPO_BRANCH when set.")
	nodeScalingRepoFilePath := flag.String("node-scaling-repo-file-path", "", "Path to the MachineDeployment manifest inside the node scaling repo. Overrides GITEA_REPO_FILE_PATH when set.")
	nodeScalingGitUsername := flag.String("node-scaling-git-username", "", "Git username for node scaling repo access. Overrides GITEA_USERNAME when set.")
	nodeScalingGitPassword := flag.String("node-scaling-git-password", "", "Git password or token for node scaling repo access. Overrides GITEA_PASSWORD when set.")
	flag.Parse()

	config, err := kubeconfig.GetKubeConfig()
	if err != nil {
		panic(err)
	}

	ichpClient, err := ichp.NewForConfig(config)
	if err != nil {
		panic(err)
	}

	client, err := kubeconfig.GetKubernetesClient()
	if err != nil {
		panic(err)
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		panic(err)
	}

	nodeScalingRuntime, err := nodescaling.InitializeNodeScaling(*enableNodeScaling, nodescaling.NodeScalingConfig{
		RepoURL:      *nodeScalingRepoURL,
		RepoBranch:   *nodeScalingRepoBranch,
		RepoFilePath: *nodeScalingRepoFilePath,
		Username:     *nodeScalingGitUsername,
		Password:     *nodeScalingGitPassword,
	})
	if err != nil {
		panic(err)
	}
	var scaleOutRequestHandler nodescaling.ScaleOutRequestHandler
	if nodeScalingRuntime != nil {
		nodeScalingController := nodescaling.NewNodeScalingController(
			nodeScalingRuntime,
			client,
			nodescaling.NewKubernetesNodeScalingInventoryStore(dynamicClient),
			time.Minute,
		)
		nodeScalingController.SetQuotaAutoscalerClient(ichpClient)
		nodeScalingController.SetScaleInTriggerDelay(*nodeScaleInDelay)
		nodeScalingController.SetScaleInForceEnabled(*enableNodeScaleInForce)
		nodeScalingController.SetScaleInForceDelay(*nodeScaleInForceDelay)
		nodeScalingController.SetScaleInExemptNamespaces(strings.Split(*nodeScaleInExemptNamespaces, ","))
		nodeScalingController.SetScaleInExemptPodMarker(*nodeScaleInExemptPodKey, *nodeScaleInExemptPodValue)
		nodeScalingController.SetMaxNodeCount(int32(*nodeScalingMaxNodes))
		nodeScalingController.SetPreparedSpareCount(int32(*nodeScalingPreparedSpares))
		nodeScalingController.SetActivatedSpareCount(int32(*nodeScalingActivatedSpares))
		scaleOutRequestHandler = nodeScalingController
		go nodeScalingController.Run()
	}
	quotaController := quota.NewQuotaController(client, *quotaCheckInterval, scaleOutRequestHandler)

	go func() {
		// Profiling
		panic(http.ListenAndServe(":8080", nil))
	}()

	// Runs forever, handles Resize events async by calling the Resize API
	go resize.RunEventHandler(*quotaUpdateInterval)

	for {
		logging.LogInfo("(re-)starting stream")
		var watchTimeoutSec int64 = 3600 // Hourly

		startScalerState, err := ichpClient.IchpV1().QuotaAutoscalers("").List(context.TODO(), v1.ListOptions{})
		if err != nil {
			exitIfQuotaAutoscalerCRDMissing(err)
			panic(err)
		}

		scalerWatch, err := ichpClient.IchpV1().QuotaAutoscalers("").Watch(context.TODO(), v1.ListOptions{TimeoutSeconds: &watchTimeoutSec})
		if err != nil {
			exitIfQuotaAutoscalerCRDMissing(err)
			panic(err)
		}

		quotaWatch, err := client.CoreV1().ResourceQuotas("").Watch(context.TODO(), v1.ListOptions{TimeoutSeconds: &watchTimeoutSec})
		if err != nil {
			panic(err)
		}

		// FailedCreate events let the quota controller react immediately when workload creation is denied by quota.
		eventWatch, err := client.CoreV1().Events("").Watch(context.TODO(), v1.ListOptions{TimeoutSeconds: &watchTimeoutSec, FieldSelector: "reason=FailedCreate"})
		if err != nil {
			panic(err)
		}

		// Blocking call until stream watch timeout
		quotaController.Run(startScalerState.Items, quotaWatch.ResultChan(), scalerWatch.ResultChan(), eventWatch.ResultChan())

		scalerWatch.Stop()
		quotaWatch.Stop()
		eventWatch.Stop()
	}
}

func exitIfQuotaAutoscalerCRDMissing(err error) {
	if !apierrors.IsNotFound(err) {
		return
	}

	fmt.Fprintln(os.Stderr, "QuotaAutoscaler CRD is not installed in the target cluster.")
	fmt.Fprintln(os.Stderr, "Install it first, for example:")
	fmt.Fprintln(os.Stderr, "  kubectl apply -f deploy/helm-quotascale-controller/templates/crd.yaml")
	fmt.Fprintf(os.Stderr, "Original error: %v\n", err)
	os.Exit(1)
}
