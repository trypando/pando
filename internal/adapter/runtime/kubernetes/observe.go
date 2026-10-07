package kubernetes

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
)

// Observe reports what exists. It never remediates (design 05 §2.1): a pod
// that exited is reported exited, and the reconciler decides.
func (a *Adapter) Observe(ctx context.Context, ref api.BundleRef) (api.ObservedBundle, error) {
	ns := namespaceFor(ref.BundleID)
	if _, err := a.cs.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{}); apierrors.IsNotFound(err) {
		return api.ObservedBundle{}, nil
	} else if err != nil {
		return api.ObservedBundle{}, errs.Wrap(errs.AdapterUnavailable, "Could not read what is running.", err)
	}

	pods, err := a.cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: labelManagedBy + "=" + managedBy})
	if err != nil {
		return api.ObservedBundle{}, errs.Wrap(errs.AdapterUnavailable, "Could not read what is running.", err)
	}
	services, err := a.cs.CoreV1().Services(ns).List(ctx, metav1.ListOptions{LabelSelector: labelManagedBy + "=" + managedBy})
	if err != nil {
		return api.ObservedBundle{}, errs.Wrap(errs.AdapterUnavailable, "Could not read what is running.", err)
	}

	byWorkload := map[string][]corev1.Pod{}
	for _, p := range pods.Items {
		w := p.Labels[labelWorkload]
		if w == "" || p.Labels[labelRole] != "" {
			// Pando's, not the app's: the gateway, a backup helper.
			continue
		}
		byWorkload[w] = append(byWorkload[w], p)
	}

	var observed api.ObservedBundle
	seen := map[string]bool{}
	for name, list := range byWorkload {
		seen[name] = true
		observed.Workloads = append(observed.Workloads, observePod(name, newest(list)))
	}
	// A stopped workload has no pod and keeps its Service: present, not
	// running, as a stopped Docker container is.
	for _, s := range services.Items {
		name := s.Labels[labelWorkload]
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		observed.Workloads = append(observed.Workloads, api.ObservedWorkload{Name: name, Present: true})
	}
	observed.Exists = len(observed.Workloads) > 0

	claims, err := a.cs.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{LabelSelector: labelManagedBy + "=" + managedBy})
	if err == nil {
		for _, c := range claims.Items {
			observed.Volumes = append(observed.Volumes, api.ObservedVolume{
				VolumeID: c.Labels[labelVolume], Present: true, Handle: ns + volumeHandleSep + c.Name,
			})
		}
	}
	return observed, nil
}

// observePod reports one workload from its newest pod.
func observePod(name string, p *corev1.Pod) api.ObservedWorkload {
	w := api.ObservedWorkload{Name: name, Present: true}
	var status *corev1.ContainerStatus
	for i := range p.Status.ContainerStatuses {
		if p.Status.ContainerStatuses[i].Name == appContainer {
			status = &p.Status.ContainerStatuses[i]
		}
	}
	if status != nil {
		w.ImageDigest = status.ImageID
		switch {
		case status.State.Running != nil:
			w.Running = p.DeletionTimestamp == nil
			w.StartedAt = status.State.Running.StartedAt.UTC()
		case status.State.Terminated != nil:
			code := int(status.State.Terminated.ExitCode)
			w.ExitCode = &code
			w.StartedAt = status.State.Terminated.StartedAt.UTC()
		}
	}
	// A pod being deleted — its node failed, or it is being replaced — is
	// not running, whatever its last status said. The reconciler recreates
	// it under a new name, and the scheduler puts it where there is room
	// (O-44).
	if p.DeletionTimestamp != nil {
		w.Running = false
	}

	// Healthy only when the workload has a probe: "no health signal" and
	// "unhealthy" are different states (R-221).
	if w.Running && len(p.Spec.Containers) > 0 && p.Spec.Containers[0].ReadinessProbe != nil {
		ready := status != nil && status.Ready
		w.Healthy = &ready
	}
	// restartPolicy Never: the kubelet never restarts it, so there is no
	// restart count and no restart loop to report (R-151).
	w.RestartCount = 0
	w.Restarting = false
	return w
}

// Stop removes the app's running pods and keeps everything else: its
// Services, its volumes, its configuration, and any pod that has already
// finished. A finished pod uses nothing and is how the last crash's output
// stays readable: the reconciler stops an app as it gives up on it (R-150),
// and deleting every pod then took the log of the crash that sent it to
// failed with it — on Docker the stopped container keeps its log. A finished
// pod cannot be started again, so a stopped app is started by Apply creating
// new pods.
func (a *Adapter) Stop(ctx context.Context, ref api.BundleRef) error {
	ns := namespaceFor(ref.BundleID)
	if err := a.deletePods(ctx, ns, false); err != nil {
		return errs.Wrap(errs.AdapterFailed, "Could not stop the app.", err)
	}
	return nil
}

var managedOnly = metav1.ListOptions{LabelSelector: labelManagedBy + "=" + managedBy}

// deleteManagedPods removes every pod Pando made in a namespace.
func (a *Adapter) deleteManagedPods(ctx context.Context, ns string) error {
	return a.deletePods(ctx, ns, true)
}

// deletePods removes the pods Pando made in a namespace: all of them, or
// only those not yet finished.
func (a *Adapter) deletePods(ctx context.Context, ns string, finished bool) error {
	list, err := a.cs.CoreV1().Pods(ns).List(ctx, managedOnly)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(list.Items))
	for _, p := range list.Items {
		if !finished && (p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed) {
			continue
		}
		names = append(names, p.Name)
	}
	return deleteEach(names, func(n string) error { return a.cs.CoreV1().Pods(ns).Delete(ctx, n, metav1.DeleteOptions{}) })
}

// deleteEach deletes by name, one at a time, ignoring what is already gone.
// One at a time rather than a collection delete, which not every kind has.
func deleteEach(names []string, del func(string) error) error {
	for _, n := range names {
		if err := del(n); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// Destroy removes an app.
//
// With KeepVolumes, the default (R-204), the pods, Services, Secrets and
// ConfigMaps go and the namespace stays with its claims, its default-deny
// policy and Pando's role binding: a claim lives in a namespace. Without it,
// each volume goes as DestroyVolume removes one, and then the namespace.
func (a *Adapter) Destroy(ctx context.Context, ref api.BundleRef, opts api.DestroyOptions) error {
	ns := namespaceFor(ref.BundleID)
	if _, err := a.cs.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{}); apierrors.IsNotFound(err) {
		return nil
	}

	if opts.KeepVolumes {
		// Nothing to keep: the namespace goes, in one delete. Keeping it
		// would leave a namespace, a policy and a role binding behind for
		// every deleted app that had no storage, and the cluster tier is
		// counted in namespaces (O-40).
		claims, err := a.cs.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return errs.Wrap(errs.AdapterUnavailable, "Could not read the app's storage.", err)
		}
		if len(claims.Items) == 0 {
			opts.KeepVolumes = false
		}
	}

	if opts.KeepVolumes {
		// Set before anything is removed, so a claim deleted later by hand,
		// or with the namespace, does not take the data with it.
		if err := a.retainVolumes(ctx, ns); err != nil {
			return err
		}
		core := a.cs.CoreV1()
		steps := []func() error{
			func() error { return a.deleteManagedPods(ctx, ns) },
			func() error {
				list, err := core.Secrets(ns).List(ctx, managedOnly)
				if err != nil {
					return err
				}
				names := make([]string, 0, len(list.Items))
				for _, s := range list.Items {
					names = append(names, s.Name)
				}
				return deleteEach(names, func(n string) error { return core.Secrets(ns).Delete(ctx, n, metav1.DeleteOptions{}) })
			},
			func() error {
				list, err := core.ConfigMaps(ns).List(ctx, managedOnly)
				if err != nil {
					return err
				}
				names := make([]string, 0, len(list.Items))
				for _, c := range list.Items {
					names = append(names, c.Name)
				}
				return deleteEach(names, func(n string) error { return core.ConfigMaps(ns).Delete(ctx, n, metav1.DeleteOptions{}) })
			},
			func() error {
				list, err := core.Services(ns).List(ctx, managedOnly)
				if err != nil {
					return err
				}
				names := make([]string, 0, len(list.Items))
				for _, s := range list.Items {
					names = append(names, s.Name)
				}
				return deleteEach(names, func(n string) error { return core.Services(ns).Delete(ctx, n, metav1.DeleteOptions{}) })
			},
			func() error {
				return a.cs.NetworkingV1().NetworkPolicies(ns).Delete(ctx, gatewayPolicy, metav1.DeleteOptions{})
			},
		}
		for _, step := range steps {
			if err := step(); err != nil && !apierrors.IsNotFound(err) {
				return errs.Wrap(errs.AdapterFailed, "Could not remove the app from the cluster.", err)
			}
		}
		return nil
	}

	claims, err := a.cs.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return errs.Wrap(errs.AdapterUnavailable, "Could not read the app's storage.", err)
	}
	for _, c := range claims.Items {
		if err := a.DestroyVolume(ctx, api.VolumeHandle{Handle: ns + volumeHandleSep + c.Name}); err != nil {
			return err
		}
	}
	err = a.cs.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return errs.Wrap(errs.AdapterFailed, "Could not remove the app's namespace from the cluster.", err)
	}
	return nil
}

// CreateVolume makes a claim in the app's namespace.
func (a *Adapter) CreateVolume(ctx context.Context, req api.VolumeRequest) (api.VolumeHandle, error) {
	if err := a.ensureNamespace(ctx, req.BundleID); err != nil {
		return api.VolumeHandle{}, err
	}
	ns := namespaceFor(req.BundleID)
	name := pvcName(req.VolumeID)
	handle := api.VolumeHandle{VolumeID: req.VolumeID, Handle: ns + volumeHandleSep + name}

	claims := a.cs.CoreV1().PersistentVolumeClaims(ns)
	if existing, err := claims.Get(ctx, name, metav1.GetOptions{}); err == nil {
		return handle, a.setReclaim(ctx, existing.Spec.VolumeName, corev1.PersistentVolumeReclaimRetain)
	} else if !apierrors.IsNotFound(err) {
		return api.VolumeHandle{}, errs.Wrap(errs.AdapterUnavailable, "Could not read the app's storage.", err)
	}

	size := req.SizeBytes
	if size <= 0 {
		size = a.config.VolumeSizeBytes
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns,
			Labels:      map[string]string{labelManagedBy: managedBy, labelBundle: toLabel(req.BundleID), labelVolume: req.VolumeID},
			Annotations: map[string]string{"pando.dev/volume-name": req.Name},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			// ReadWriteOnce, never ReadWriteOncePod: a backup helper must be
			// able to mount the claim beside the running app.
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceStorage: *resource.NewQuantity(size, resource.BinarySI),
			}},
		},
	}
	if a.config.StorageClass != "" {
		pvc.Spec.StorageClassName = ptr.To(a.config.StorageClass)
	}
	if _, err := claims.Create(ctx, pvc, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return api.VolumeHandle{}, errs.Wrap(errs.AdapterFailed, "Could not create storage for the app.", err)
	}
	return handle, nil
}

// DestroyVolume deletes a volume and its data. The volume's reclaim policy is
// set back to Delete first: deleting a retained volume's object would leave
// the disk behind it, which is not what destroying a volume means.
func (a *Adapter) DestroyVolume(ctx context.Context, h api.VolumeHandle) error {
	ns, name, ok := strings.Cut(h.Handle, volumeHandleSep)
	if !ok {
		return errs.Newf(errs.ValidInvalid, "%q is not storage the Kubernetes runtime made.", h.Handle)
	}
	claims := a.cs.CoreV1().PersistentVolumeClaims(ns)
	existing, err := claims.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return errs.Wrap(errs.AdapterUnavailable, "Could not read the app's storage.", err)
	}
	if err := a.setReclaim(ctx, existing.Spec.VolumeName, corev1.PersistentVolumeReclaimDelete); err != nil {
		return err
	}
	if err := claims.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return errs.Wrap(errs.AdapterFailed, "Could not remove the storage.", err)
	}
	return nil
}

// Logs streams a workload's newest pod's log, a finished one included, so the
// crash that sent an app to failed can still be read.
func (a *Adapter) Logs(ctx context.Context, ref api.WorkloadRef, opts api.LogOptions) (io.ReadCloser, error) {
	ns := namespaceFor(ref.BundleID)
	pods, err := a.workloadPods(ctx, ns, ref.Workload)
	if err != nil {
		return nil, err
	}
	p := newest(pods)
	if p == nil {
		return nil, errs.Newf(errs.NotFound, "There is nothing running called %q.", ref.Workload)
	}
	logOpts := &corev1.PodLogOptions{Container: appContainer, Follow: opts.Follow}
	if !opts.Since.IsZero() {
		logOpts.SinceTime = &metav1.Time{Time: opts.Since}
	}
	if opts.Tail > 0 {
		logOpts.TailLines = ptr.To(int64(opts.Tail))
	}
	rc, err := a.cs.CoreV1().Pods(ns).GetLogs(p.Name, logOpts).Stream(ctx)
	if err != nil {
		return nil, errs.Wrap(errs.AdapterFailed, "Could not read the app's logs.", err)
	}
	return rc, nil
}

// Upstream is the workload's Service name inside the cluster (R-023).
//
// Computed, not looked up: it runs on every proxied request. A name rather
// than an address, so it survives the pod being recreated. Only Pando's server
// pods may connect to it (ensurePolicies), and nothing outside the cluster can
// resolve it (R-026).
func (a *Adapter) Upstream(_ context.Context, ref api.WorkloadRef, port int) (api.Upstream, error) {
	if port <= 0 || port > 65535 {
		return api.Upstream{}, errs.Newf(errs.ValidInvalid, "%d is not a usable port.", port)
	}
	if err := checkWorkloadName(ref.Workload); err != nil {
		return api.Upstream{}, err
	}
	return api.Upstream{URL: fmt.Sprintf("http://%s.%s.svc.%s:%d",
		ref.Workload, namespaceFor(ref.BundleID), a.config.ClusterDomain, port)}, nil
}

func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}
