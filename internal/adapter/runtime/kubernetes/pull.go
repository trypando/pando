package kubernetes

import (
	"errors"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/trypando/pando/internal/registrylimit"
)

// pullLimited is the refusal for a pod whose app container a node could not
// pull because the registry is limiting downloads, nil otherwise.
//
// The kubelet reports it as ErrImagePull, then ImagePullBackOff, with the
// registry's 429 in the message. Said in the same words as the registry read
// before a deploy and the Docker runtime's pull (R-105), rather than left as a
// pod waiting with nothing reported.
func pullLimited(p *corev1.Pod, signed bool) error {
	for _, s := range p.Status.ContainerStatuses {
		w := s.State.Waiting
		if s.Name != appContainer || w == nil || (w.Reason != "ErrImagePull" && w.Reason != "ImagePullBackOff") {
			continue
		}
		if !registrylimit.Mentioned(w.Message) {
			return nil
		}
		image := s.Image
		for _, c := range p.Spec.Containers {
			if c.Name == appContainer && image == "" {
				image = c.Image
			}
		}
		r := registrylimit.FromText(w.Message, image)
		r.Signed = registrylimit.Anonymous
		if signed {
			r.Signed = registrylimit.SignedIn
		}
		return r.Error(time.Now(), errors.New(w.Message))
	}
	return nil
}
