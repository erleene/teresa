package k8s

import (
	"fmt"
	"strconv"

	"github.com/luizalabs/teresa/pkg/server/app"
	"github.com/luizalabs/teresa/pkg/server/service"
	"github.com/luizalabs/teresa/pkg/server/spec"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	k8sv1 "k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/pkg/apis/apps/v1beta1"
	k8sbatch "k8s.io/client-go/pkg/apis/batch/v1"
	k8sv2alpha "k8s.io/client-go/pkg/apis/batch/v2alpha1"
	k8s_extensions "k8s.io/client-go/pkg/apis/extensions/v1beta1"
)

const (
	changeCauseAnnotation = "kubernetes.io/change-cause"
	appTypeAnnotation     = "teresa.io/app-type"
	defaultServicePort    = 80
)

func podSpecToK8sContainers(podSpec *spec.Pod) ([]k8sv1.Container, error) {
	return containerSpecsToK8sContainers(podSpec.Containers)
}

func containerSpecsToK8sContainers(containerSpecs []*spec.Container) ([]k8sv1.Container, error) {
	containers := make([]k8sv1.Container, len(containerSpecs))
	for i, cs := range containerSpecs {
		c := k8sv1.Container{
			Name:            cs.Name,
			ImagePullPolicy: k8sv1.PullAlways,
			Image:           cs.Image,
		}

		if cs.ContainerLimits != nil {
			cpu, err := resource.ParseQuantity(cs.ContainerLimits.CPU)
			if err != nil {
				return nil, err
			}
			memory, err := resource.ParseQuantity(cs.ContainerLimits.Memory)
			if err != nil {
				return nil, err
			}
			c.Resources = k8sv1.ResourceRequirements{
				Limits: k8sv1.ResourceList{
					k8sv1.ResourceCPU:    cpu,
					k8sv1.ResourceMemory: memory,
				},
			}
		}

		if len(cs.Command) > 0 {
			c.Command = cs.Command
		}
		c.Args = append(c.Args, cs.Args...)

		for k, v := range cs.Env {
			c.Env = append(c.Env, k8sv1.EnvVar{Name: k, Value: v})
		}
		for _, secret := range cs.Secrets {
			c.Env = append(c.Env, k8sv1.EnvVar{
				Name: secret,
				ValueFrom: &k8sv1.EnvVarSource{
					SecretKeyRef: &k8sv1.SecretKeySelector{
						Key: secret,
						LocalObjectReference: k8sv1.LocalObjectReference{
							Name: app.TeresaAppSecrets,
						},
					},
				},
			})
		}
		for _, vm := range cs.VolumeMounts {
			c.VolumeMounts = append(c.VolumeMounts, k8sv1.VolumeMount{
				Name:      vm.Name,
				MountPath: vm.MountPath,
				ReadOnly:  vm.ReadOnly,
			})
		}
		for _, p := range cs.Ports {
			c.Ports = append(c.Ports, k8sv1.ContainerPort{
				Name:          p.Name,
				ContainerPort: p.ContainerPort,
			})
		}
		containers[i] = c
	}
	return containers, nil
}

func podSpecVolumesToK8sVolumes(vols []*spec.Volume) []k8sv1.Volume {
	volumes := make([]k8sv1.Volume, 0)
	for _, v := range vols {
		vol := k8sv1.Volume{Name: v.Name}
		if v.EmptyDir {
			vol.EmptyDir = &k8sv1.EmptyDirVolumeSource{}
		} else if v.SecretName != "" {
			vol.Secret = &k8sv1.SecretVolumeSource{
				SecretName: v.SecretName,
			}
		} else if v.ConfigMapName != "" {
			vol.ConfigMap = &k8sv1.ConfigMapVolumeSource{}
			vol.ConfigMap.Name = v.ConfigMapName
		}
		volumes = append(volumes, vol)
	}
	return volumes
}

func podSpecToK8sPod(podSpec *spec.Pod) (*k8sv1.Pod, error) {
	containers, err := podSpecToK8sContainers(podSpec)
	if err != nil {
		return nil, err
	}
	volumes := podSpecVolumesToK8sVolumes(podSpec.Volumes)
	f := false

	initContainers, err := podSpecToK8sInitContainers(podSpec)
	if err != nil {
		return nil, err
	}

	ps := k8sv1.PodSpec{
		RestartPolicy: k8sv1.RestartPolicyNever,
		Containers:    containers,
		Volumes:       volumes,
		AutomountServiceAccountToken: &f,
		InitContainers:               initContainers,
	}

	pod := &k8sv1.Pod{
		TypeMeta: metav1.TypeMeta{Kind: "Pod", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      podSpec.Name,
			Namespace: podSpec.Namespace,
		},
		Spec: ps,
	}
	return pod, nil
}

func deploySpecToK8sDeploy(deploySpec *spec.Deploy, replicas int32) (*v1beta1.Deployment, error) {
	containers, err := podSpecToK8sContainers(&deploySpec.Pod)
	if err != nil {
		return nil, err
	}
	volumes := podSpecVolumesToK8sVolumes(deploySpec.Volumes)

	if deploySpec.HealthCheck != nil {
		if deploySpec.HealthCheck.Liveness != nil {
			containers[0].LivenessProbe = healthCheckProbeToK8sProbe(deploySpec.HealthCheck.Liveness)
		}
		if deploySpec.HealthCheck.Readiness != nil {
			containers[0].ReadinessProbe = healthCheckProbeToK8sProbe(deploySpec.HealthCheck.Readiness)
		}
	}

	if deploySpec.Lifecycle != nil {
		containers[0].Lifecycle = lifecycleToK8sLifecycle(deploySpec.Lifecycle)
	}

	f := false
	initContainers, err := podSpecToK8sInitContainers(&deploySpec.Pod)
	if err != nil {
		return nil, err
	}
	ps := k8sv1.PodSpec{
		RestartPolicy: k8sv1.RestartPolicyAlways,
		Containers:    containers,
		Volumes:       volumes,
		AutomountServiceAccountToken: &f,
		InitContainers:               initContainers,
	}

	var maxSurge, maxUnavailable *intstr.IntOrString
	if deploySpec.RollingUpdate != nil {
		vMaxSurge, vMaxUnavailable := rollingUpdateToK8sRollingUpdate(deploySpec.RollingUpdate)
		maxSurge, maxUnavailable = &vMaxSurge, &vMaxUnavailable
	}

	rhl := int32(deploySpec.RevisionHistoryLimit)
	d := &v1beta1.Deployment{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "extensions/v1beta1",
			Kind:       "Deployment",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      deploySpec.Name,
			Namespace: deploySpec.Namespace,
			Labels:    map[string]string{"run": deploySpec.Name},
			Annotations: map[string]string{
				changeCauseAnnotation: deploySpec.Description,
				spec.SlugAnnotation:   deploySpec.SlugURL,
			},
		},
		Spec: v1beta1.DeploymentSpec{
			Replicas: &replicas,
			Strategy: v1beta1.DeploymentStrategy{
				Type: v1beta1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &v1beta1.RollingUpdateDeployment{
					MaxUnavailable: maxUnavailable,
					MaxSurge:       maxSurge,
				},
			},
			Template: k8sv1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"run": deploySpec.Name},
				},
				Spec: ps,
			},
			RevisionHistoryLimit: &rhl,
		},
	}
	return d, nil
}

func podSpecToK8sInitContainers(podSpec *spec.Pod) ([]k8sv1.Container, error) {
	return containerSpecsToK8sContainers(podSpec.InitContainers)
}

func cronJobSpecToK8sCronJob(cronJobSpec *spec.CronJob) (*k8sv2alpha.CronJob, error) {
	containers, err := podSpecToK8sContainers(&cronJobSpec.Pod)
	if err != nil {
		return nil, err
	}
	volumes := podSpecVolumesToK8sVolumes(cronJobSpec.Volumes)

	initContainers, err := podSpecToK8sInitContainers(&cronJobSpec.Pod)
	if err != nil {
		return nil, err
	}

	f := false
	ps := k8sv1.PodSpec{
		RestartPolicy: k8sv1.RestartPolicyNever,
		Containers:    containers,
		Volumes:       volumes,
		AutomountServiceAccountToken: &f,
		InitContainers:               initContainers,
	}

	successfulLim := cronJobSpec.SuccessfulJobsHistoryLimit
	failedLim := cronJobSpec.FailedJobsHistoryLimit
	cj := &k8sv2alpha.CronJob{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "batch/v2alpha1",
			Kind:       "CronJob",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      cronJobSpec.Name,
			Namespace: cronJobSpec.Namespace,
			Annotations: map[string]string{
				changeCauseAnnotation: cronJobSpec.Description,
				spec.SlugAnnotation:   cronJobSpec.SlugURL,
				appTypeAnnotation:     "cronjob",
			},
		},
		Spec: k8sv2alpha.CronJobSpec{
			Schedule: cronJobSpec.Schedule,
			JobTemplate: k8sv2alpha.JobTemplateSpec{
				Spec: k8sbatch.JobSpec{
					Template: k8sv1.PodTemplateSpec{
						Spec: ps,
					},
				},
			},
			SuccessfulJobsHistoryLimit: &successfulLim,
			FailedJobsHistoryLimit:     &failedLim,
		},
	}
	return cj, nil
}

func rollingUpdateToK8sRollingUpdate(ru *spec.RollingUpdate) (maxSurge, maxUnavailable intstr.IntOrString) {
	conv := func(value string) intstr.IntOrString {
		v, err := strconv.Atoi(value)
		if err != nil {
			return intstr.FromString(value)
		}
		return intstr.FromInt(v)
	}
	return conv(ru.MaxSurge), conv(ru.MaxUnavailable)
}

func healthCheckProbeToK8sProbe(probe *spec.HealthCheckProbe) *k8sv1.Probe {
	return &k8sv1.Probe{
		InitialDelaySeconds: probe.InitialDelaySeconds,
		TimeoutSeconds:      probe.TimeoutSeconds,
		PeriodSeconds:       probe.PeriodSeconds,
		FailureThreshold:    probe.FailureThreshold,
		SuccessThreshold:    probe.SuccessThreshold,
		Handler: k8sv1.Handler{
			HTTPGet: &k8sv1.HTTPGetAction{
				Port: intstr.FromInt(spec.DefaultPort),
				Path: probe.Path,
			},
		},
	}
}

func lifecycleToK8sLifecycle(lc *spec.Lifecycle) *k8sv1.Lifecycle {
	k8sLc := new(k8sv1.Lifecycle)

	if lc.PreStop != nil {
		k8sLc.PreStop = &k8sv1.Handler{
			Exec: &k8sv1.ExecAction{
				Command: []string{"/bin/sleep", strconv.Itoa(lc.PreStop.DrainTimeoutSeconds)},
			},
		}
	}

	return k8sLc
}

func serviceSpec(namespace, name, srvType string) *k8sv1.Service {
	serviceType := k8sv1.ServiceType(srvType)
	return &k8sv1.Service{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Service",
		},
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				"run": name,
			},
			Name:      name,
			Namespace: namespace,
		},
		Spec: k8sv1.ServiceSpec{
			Type:            serviceType,
			SessionAffinity: k8sv1.ServiceAffinityNone,
			Selector: map[string]string{
				"run": name,
			},
			Ports: []k8sv1.ServicePort{
				{
					Port:       80,
					Protocol:   k8sv1.ProtocolTCP,
					TargetPort: intstr.FromInt(spec.DefaultPort),
				},
			},
		},
	}
}

func ingressSpec(namespace, name, vHost string) *k8s_extensions.Ingress {
	return &k8s_extensions.Ingress{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "extensions/v1beta1",
			Kind:       "Ingress",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: k8s_extensions.IngressSpec{
			Rules: []k8s_extensions.IngressRule{
				{
					Host: vHost,
					IngressRuleValue: k8s_extensions.IngressRuleValue{
						HTTP: &k8s_extensions.HTTPIngressRuleValue{
							Paths: []k8s_extensions.HTTPIngressPath{
								{
									Path: "/",
									Backend: k8s_extensions.IngressBackend{
										ServiceName: name,
										ServicePort: intstr.FromInt(defaultServicePort),
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

func appPodListOptsToK8s(opts *app.PodListOptions) *metav1.ListOptions {
	var k8sOpts metav1.ListOptions

	if opts.PodName != "" {
		k8sOpts.FieldSelector = fmt.Sprintf("metadata.name=%s", opts.PodName)
	}

	return &k8sOpts
}

func configMapSpec(namespace, name string, data map[string]string) *k8sv1.ConfigMap {
	return &k8sv1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "ConfigMap",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Data: data,
	}
}

func servicePortsToK8sServicePorts(ports []service.ServicePort) []k8sv1.ServicePort {
	k8sPorts := make([]k8sv1.ServicePort, len(ports))
	for i := range ports {
		p := ports[i].Port
		if p == 0 {
			p = defaultServicePort
		}
		k8sPorts[i] = k8sv1.ServicePort{
			Name:       ports[i].Name,
			Port:       int32(p),
			TargetPort: intstr.FromInt(ports[i].TargetPort),
		}
	}
	return k8sPorts
}
