module infinibox-csi-driver

go 1.15

require (
	bou.ke/monkey v1.0.2
	github.com/container-storage-interface/spec v1.4.0
	github.com/containerd/containerd v1.4.4
	github.com/containerd/continuity v0.1.0 // indirect
	github.com/go-resty/resty/v2 v2.6.0
	github.com/golang/protobuf v1.5.2
	github.com/google/uuid v1.2.0 // indirect
	github.com/rexray/gocsi v1.2.2
	github.com/stretchr/testify v1.7.0
	google.golang.org/grpc v1.27.1
	google.golang.org/protobuf v1.26.0
	k8s.io/api v0.21.0
	k8s.io/apimachinery v0.21.1-rc.0
	k8s.io/client-go v0.21.0
	k8s.io/klog v1.0.0
	k8s.io/kubernetes v1.21.0
	k8s.io/mount-utils v0.21.1-rc.0
	k8s.io/utils v0.0.0-20210305010621-2afb4311ab10
)

replace k8s.io/api => k8s.io/api v0.21.0

replace k8s.io/apimachinery => k8s.io/apimachinery v0.21.1-rc.0

replace k8s.io/client-go => k8s.io/client-go v0.21.0

replace k8s.io/mount-utils => k8s.io/mount-utils v0.21.1-rc.0

replace k8s.io/apiextensions-apiserver => k8s.io/apiextensions-apiserver v0.21.0

replace k8s.io/apiserver => k8s.io/apiserver v0.21.0

replace k8s.io/cli-runtime => k8s.io/cli-runtime v0.21.0

replace k8s.io/cloud-provider => k8s.io/cloud-provider v0.21.0

replace k8s.io/cluster-bootstrap => k8s.io/cluster-bootstrap v0.21.0

replace k8s.io/code-generator => k8s.io/code-generator v0.21.1-rc.0

replace k8s.io/component-base => k8s.io/component-base v0.21.0

replace k8s.io/component-helpers => k8s.io/component-helpers v0.21.0

replace k8s.io/controller-manager => k8s.io/controller-manager v0.21.0

replace k8s.io/cri-api => k8s.io/cri-api v0.21.1-rc.0

replace k8s.io/csi-translation-lib => k8s.io/csi-translation-lib v0.21.0

replace k8s.io/kube-aggregator => k8s.io/kube-aggregator v0.21.0

replace k8s.io/kube-controller-manager => k8s.io/kube-controller-manager v0.21.0

replace k8s.io/kube-proxy => k8s.io/kube-proxy v0.21.0

replace k8s.io/kube-scheduler => k8s.io/kube-scheduler v0.21.0

replace k8s.io/kubectl => k8s.io/kubectl v0.21.0

replace k8s.io/kubelet => k8s.io/kubelet v0.21.0

replace k8s.io/legacy-cloud-providers => k8s.io/legacy-cloud-providers v0.21.0

replace k8s.io/metrics => k8s.io/metrics v0.21.0

replace k8s.io/sample-apiserver => k8s.io/sample-apiserver v0.21.0

replace k8s.io/sample-cli-plugin => k8s.io/sample-cli-plugin v0.21.0

replace k8s.io/sample-controller => k8s.io/sample-controller v0.21.0
