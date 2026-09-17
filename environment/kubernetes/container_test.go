package kubernetes

import (
	"testing"

	. "github.com/franela/goblin"
	corev1 "k8s.io/api/core/v1"

	"github.com/pelican/wings/config"
)

// TestResolveImagePullPolicy verifies pull-policy resolution for remote
// images, ~-prefixed local images, and configured overrides.
func TestResolveImagePullPolicy(t *testing.T) {
	g := Goblin(t)

	g.Describe("resolveImagePullPolicy", func() {
		g.BeforeEach(func() {
			config.Update(func(c *config.Configuration) {
				c.Kubernetes.ImagePullPolicy = ""
			})
		})

		g.It("always pulls remote images so updated tags are picked up", func() {
			image, policy := resolveImagePullPolicy("ghcr.io/pelican-eggs/games:latest")
			g.Assert(image).Equal("ghcr.io/pelican-eggs/games:latest")
			g.Assert(policy).Equal(corev1.PullAlways)
		})

		g.It("does not pull ~-prefixed local images and strips the prefix", func() {
			image, policy := resolveImagePullPolicy("~local/custom:dev")
			g.Assert(image).Equal("local/custom:dev")
			g.Assert(policy).Equal(corev1.PullIfNotPresent)
		})

		g.It("honors a configured override for remote images", func() {
			config.Update(func(c *config.Configuration) {
				c.Kubernetes.ImagePullPolicy = "IfNotPresent"
			})
			image, policy := resolveImagePullPolicy("ghcr.io/pelican-eggs/games:latest")
			g.Assert(image).Equal("ghcr.io/pelican-eggs/games:latest")
			g.Assert(policy).Equal(corev1.PullIfNotPresent)
		})

		g.It("honors a configured override and still strips the local prefix", func() {
			config.Update(func(c *config.Configuration) {
				c.Kubernetes.ImagePullPolicy = "Never"
			})
			image, policy := resolveImagePullPolicy("~local/custom:dev")
			g.Assert(image).Equal("local/custom:dev")
			g.Assert(policy).Equal(corev1.PullNever)
		})
	})
}
