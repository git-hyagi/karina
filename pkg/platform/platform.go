package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"strings"
	"time"

	"github.com/gobuffalo/packd"
	"github.com/gobuffalo/packr/v2"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"
	v1 "k8s.io/api/core/v1"
	"k8s.io/api/networking/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	kapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/moshloop/commons/deps"
	"github.com/moshloop/commons/files"
	"github.com/moshloop/commons/is"
	"github.com/moshloop/commons/text"
	konfigadm "github.com/moshloop/konfigadm/pkg/types"
	"github.com/moshloop/platform-cli/pkg/api"
	"github.com/moshloop/platform-cli/pkg/client/dns"
	"github.com/moshloop/platform-cli/pkg/k8s"
	"github.com/moshloop/platform-cli/pkg/provision/vmware"
	"github.com/moshloop/platform-cli/pkg/types"
	"github.com/moshloop/platform-cli/pkg/utils"
)

type Platform struct {
	types.PlatformConfig
	k8s.Client
	ctx     context.Context
	session *vmware.Session
}

func (platform *Platform) Init() {
	platform.Client.GetKubeConfig = platform.GetKubeConfig
}

// GetVMs returns a list of all VM's associated with the cluster
func (platform *Platform) GetVMs() (map[string]*VM, error) {
	var vms = make(map[string]*VM)
	list, err := platform.session.Finder.VirtualMachineList(
		platform.ctx, fmt.Sprintf("%s-%s-*", platform.HostPrefix, platform.Name))
	if err != nil {
		return nil, err
	}
	for _, vm := range list {
		item := &VM{
			Platform: platform,
			ctx:      platform.ctx,
			vm:       vm,
		}
		item.Name = vm.Name()
		vms[vm.Name()] = item
	}
	return vms, nil
}

// WaitFor at least 1 master IP to be reachable
func (platform *Platform) WaitFor() error {
	for {
		if len(platform.GetMasterIPs()) > 0 {
			return nil
		}
		time.Sleep(5 * time.Second)
	}
}

func (platform *Platform) GetDNSClient() dns.DNSClient {
	if platform.DNS != nil || platform.DNS.Disabled {
		return dns.DummyDNSClient{Zone: platform.DNS.Zone}
	}
	return dns.DynamicDNSClient{
		Zone:       platform.DNS.Zone,
		KeyName:    platform.DNS.KeyName,
		Nameserver: platform.DNS.Nameserver,
		Key:        platform.DNS.Key,
		Algorithm:  platform.DNS.Algorithm,
	}
}

func (platform *Platform) Clone(vm types.VM, config *konfigadm.Config) (string, error) {
	return platform.session.Clone(vm, config)
}

// OpenViaEnv opens a new vmware session using environment variables
func (platform *Platform) OpenViaEnv() error {
	if platform.session != nil {
		return nil
	}
	platform.ctx = context.TODO()
	session, err := vmware.GetSessionFromEnv()
	if err != nil {
		return err
	}
	platform.session = session
	return nil
}

// GetMasterIPs returns a list of healthy master IP's
func (platform *Platform) GetMasterIPs() []string {
	url := fmt.Sprintf("http://%s/v1/health/service/%s", platform.Consul, platform.Name)
	log.Infof("Finding masters via consul: %s\n", url)
	response, _ := utils.GET(url)
	var consul api.Consul
	if err := json.Unmarshal(response, &consul); err != nil {
		fmt.Println(err)
	}
	var addresses []string
node:
	for _, node := range consul {
		for _, check := range node.Checks {
			if check.Status != "passing" {
				log.Tracef("skipping unhealthy node %s -> %s", node.Node.Address, check.Status)
				continue node
			}
		}
		addresses = append(addresses, node.Node.Address)
	}
	return addresses
}

// GetKubeConfig gets the path to the admin kubeconfig, creating it if necessary
func (platform *Platform) GetKubeConfig() (string, error) {
	if os.Getenv("KUBECONFIG") != "" {
		log.Debugf("Using KUBECONFIG from ENV\n")
		return os.Getenv("KUBECONFIG"), nil
	}
	name := platform.Name + "-admin.yml"
	if !is.File(name) {
		data, err := CreateKubeConfig(platform, platform.GetMasterIPs()[0])
		if err != nil {
			return "", err
		}
		if err := ioutil.WriteFile(name, data, 0644); err != nil {
			return "", err
		}
	}
	return name, nil
}

func (platform *Platform) GetKubectl() deps.BinaryFunc {
	kubeconfig, err := platform.GetKubeConfig()
	if err != nil {
		return func(msg string, args ...interface{}) error {
			return fmt.Errorf("cannot create kubeconfig %v\n", err)
		}
	}
	if platform.DryRun {
		return platform.GetBinary("kubectl")
	}

	log.Infof("Using KUBECONFIG=%s", kubeconfig)
	return deps.BinaryWithEnv("kubectl", platform.Kubernetes.Version, ".bin", map[string]string{
		"KUBECONFIG": kubeconfig,
	})
}

// GetSecret returns the data of a secret or nil for any error
func (platform *Platform) GetSecret(namespace, name string) *map[string][]byte {
	k8s, err := platform.GetClientset()
	if err != nil {
		log.Tracef("Failed to get client %v", err)
		return nil
	}
	secret, err := k8s.CoreV1().Secrets(namespace).Get(name, metav1.GetOptions{})
	if err != nil {
		log.Tracef("Failed tp get secret %s/%s: %v\n", namespace, name, err)
		return nil
	}
	return &secret.Data
}

// GetSecret returns the data of a secret or nil for any error
func (platform *Platform) GetConfigMap(namespace, name string) *map[string]string {
	k8s, err := platform.GetClientset()
	if err != nil {
		log.Tracef("Failed to get client %v", err)
		return nil
	}
	cm, err := k8s.CoreV1().ConfigMaps(namespace).Get(name, metav1.GetOptions{})
	if err != nil {
		log.Tracef("Failed tp get secret %s/%s: %v\n", namespace, name, err)
		return nil
	}
	return &cm.Data
}

// CreateKubeConfig creates a new kubeconfig for the cluster
func CreateKubeConfig(platform *Platform, endpoint string) ([]byte, error) {
	userName := "kubernetes-admin"
	contextName := fmt.Sprintf("%s@%s", userName, platform.Name)
	cert, err := platform.Certificates.CA.ToCert().CreateCertificate("system:masters", "system:masters")
	if err != nil {
		return nil, err
	}
	cfg := kapi.Config{
		Clusters: map[string]*kapi.Cluster{
			platform.Name: {
				Server:                   "https://" + endpoint + ":6443",
				CertificateAuthorityData: []byte(platform.Certificates.CA.X509),
			},
		},
		Contexts: map[string]*kapi.Context{
			contextName: {
				Cluster:  platform.Name,
				AuthInfo: userName,
			},
		},
		AuthInfos: map[string]*kapi.AuthInfo{
			userName: {
				ClientKeyData:         cert.EncodedPrivateKey(),
				ClientCertificateData: cert.EncodedCertificate(),
			},
		},
		CurrentContext: contextName,
	}

	return clientcmd.Write(cfg)
}

// GetDynamicClient creates a new k8s client
func (platform *Platform) GetDynamicClient() (dynamic.Interface, error) {
	kubeconfig, err := platform.GetKubeConfig()
	if err != nil {
		return nil, err
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, err
	}
	return dynamic.NewForConfig(cfg)
}

// GetClientset creates a new k8s client
func (platform *Platform) GetClientset() (*kubernetes.Clientset, error) {
	kubeconfig, err := platform.GetKubeConfig()
	if err != nil {
		return nil, err
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfigOrDie(cfg), nil
}

func (platform *Platform) Template(file string) (string, error) {
	// set up a new box by giving it a name and an optional (relative) path to a folder on disk:
	box := packr.New("manifests", "../../manifests")
	raw, err := box.FindString(file)
	if err != nil {
		return "", err
	}
	template, err := text.Template(raw, platform.PlatformConfig)
	if err != nil {
		data, _ := yaml.Marshal(platform.PlatformConfig)
		log.Debugln(string(data))
		return "", err
	}
	return template, nil
}

func (platform *Platform) TemplateDir(dir string) (string, error) {
	// set up a new box by giving it a name and an optional (relative) path to a folder on disk:
	box := packr.New(dir, "../../manifests/"+dir)
	tmp, _ := ioutil.TempDir("", "template")

	if err := box.Walk(func(path string, file packd.File) error {
		to := tmp + "/" + path
		log.Debugf("Extracting %s\n", to)
		info, _ := file.FileInfo()
		_, err := files.CopyFromReader(file, to, info.Mode())
		if err != nil {
			log.Errorf("Error extracting %s: %v\n", path, err)
			return err
		}
		if err != nil {
			return err
		}
		return nil
	}); err != nil {
		return "", err
	}
	dst := ".manifests/" + dir
	os.RemoveAll(dst)
	os.MkdirAll(dst, 0775)
	return dst, text.TemplateDir(tmp, dst, platform.PlatformConfig)
}

func (platform *Platform) Annotate(objectType, name, namespace string, annotations map[string]string) error {
	if len(annotations) == 0 {
		return nil
	}
	kubectl := platform.GetKubectl()
	if namespace != "" {
		namespace = "-n " + namespace
	}

	var (
		line  string
		lines []string
	)

	for k, v := range annotations {
		line = fmt.Sprintf("%s=\"%s\"", k, v)
		lines = append(lines, line)
	}

	return kubectl("annotate %s %s %s %s", objectType, name, strings.Join(lines, " "), namespace)
}

func (platform *Platform) CreateOrUpdateSecret(name, ns string, data map[string][]byte) error {
	k8s, err := platform.GetClientset()
	if err != nil {
		return err
	}
	configs := k8s.CoreV1().Secrets(ns)
	cm, err := configs.Get(name, metav1.GetOptions{})
	if cm == nil || err != nil {
		cm = &v1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Data:       data,
		}
		log.Infof("Creating %s/secret/%s", ns, name)
		if !platform.DryRun {
			if _, err := configs.Create(cm); err != nil {
				return err
			}
		}
	} else {
		(*cm).Data = data
		if !platform.DryRun {
			log.Infof("Updating %s/secret/%s", ns, name)
			if _, err := configs.Update(cm); err != nil {
				return err
			}
		}
	}
	return nil
}

func (platform *Platform) CreateOrUpdateConfigMap(name, ns string, data map[string]string) error {
	k8s, err := platform.GetClientset()
	if err != nil {
		return err
	}
	configs := k8s.CoreV1().ConfigMaps(ns)
	cm, err := configs.Get(name, metav1.GetOptions{})
	if cm == nil || err != nil {
		cm = &v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Data:       data}
		log.Infof("Creating %s/cm/%s", ns, name)
		if !platform.DryRun {
			if _, err := configs.Create(cm); err != nil {
				return err
			}
		}
	} else {
		(*cm).Data = data
		if !platform.DryRun {
			log.Infof("Updating %s/cm/%s", ns, name)
			if _, err := configs.Update(cm); err != nil {
				return err
			}
		}
	}
	return nil
}

func (platform *Platform) ExposeIngressTLS(namespace, service string, port int) error {
	k8s, err := platform.GetClientset()
	if err != nil {
		return err
	}
	domain := fmt.Sprintf("%s.%s", service, platform.Domain)
	ingresses := k8s.NetworkingV1beta1().Ingresses(namespace)
	ingress, err := ingresses.Get(service, metav1.GetOptions{})
	if ingress == nil || err != nil {
		ingress = &v1beta1.Ingress{
			ObjectMeta: metav1.ObjectMeta{
				Name:      service,
				Namespace: namespace,
				Annotations: map[string]string{
					"nginx.ingress.kubernetes.io/ssl-passthrough": "true",
				},
			},
			Spec: v1beta1.IngressSpec{
				TLS: []v1beta1.IngressTLS{
					v1beta1.IngressTLS{
						Hosts: []string{domain},
					},
				},
				Rules: []v1beta1.IngressRule{
					v1beta1.IngressRule{
						Host: domain,
						IngressRuleValue: v1beta1.IngressRuleValue{
							HTTP: &v1beta1.HTTPIngressRuleValue{
								Paths: []v1beta1.HTTPIngressPath{
									v1beta1.HTTPIngressPath{
										Backend: v1beta1.IngressBackend{
											ServiceName: service,
											ServicePort: intstr.FromInt(port),
										},
									},
								},
							},
						},
					},
				},
			},
		}
		log.Infof("Creating %s/ingress/%s", namespace, service)
		if !platform.DryRun {
			if _, err := ingresses.Create(ingress); err != nil {
				return err
			}
		}
	}
	return nil
}

func (platform *Platform) Apply(namespace string, specs ...k8s.CRD) error {
	kubectl := platform.GetKubectl()
	if namespace != "" {
		namespace = "-n " + namespace
	}

	for _, spec := range specs {
		data, err := yaml.Marshal(spec)
		if err != nil {
			return err
		}

		log.Debugf("Applying %s\n", string(data))

		file := text.ToFile(string(data), ".yml")
		if err := kubectl("apply %s -f %s", namespace, file); err != nil {
			return err
		}
	}
	return nil
}

func (platform *Platform) ApplyText(namespace string, specs ...string) error {
	kubectl := platform.GetKubectl()
	if namespace != "" {
		namespace = "-n " + namespace
	}

	for _, spec := range specs {
		file := text.ToFile(spec, ".yml")
		if err := kubectl("apply %s -f %s", namespace, file); err != nil {
			return err
		}
	}
	return nil
}

func (platform *Platform) ApplySpecs(namespace string, specs ...string) error {
	kubectl := platform.GetKubectl()
	if namespace != "" {
		namespace = "-n " + namespace
	}
	for _, spec := range specs {
		if strings.HasSuffix(spec, "/") {
			dir, err := platform.TemplateDir(spec)
			if err != nil {
				return err
			}
			if err := kubectl("apply %s -f %s", namespace, dir); err != nil {
				return err
			}
		} else {
			template, err := platform.Template(spec)
			if err != nil {
				return err
			}
			if err := kubectl("apply %s -f %s", namespace, text.ToFile(template, ".yaml")); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *Platform) GetBinaryWithEnv(name string, env map[string]string) deps.BinaryFunc {
	if p.DryRun {
		return func(msg string, args ...interface{}) error {
			fmt.Printf("CMD: "+fmt.Sprintf("%s", env)+" .bin/"+name+" "+msg+"\n", args...)
			return nil
		}
	}
	return deps.BinaryWithEnv(name, p.Versions[name], ".bin", env)
}

func (p *Platform) GetBinary(name string) deps.BinaryFunc {
	if p.DryRun {
		return func(msg string, args ...interface{}) error {
			fmt.Printf("CMD: .bin/"+name+" "+msg+"\n", args...)
			return nil
		}
	}
	return deps.Binary(name, p.Versions[name], ".bin")
}
