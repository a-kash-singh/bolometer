package uploader

// Regression test that only uses APIs that also exist in the original
// uploader, so it can be copied onto the original code.

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/a-kash-singh/bolometer/internal/profiler"
)

// Replicas of one service are captured at the same moment (captures run
// concurrently). Their profiles must get distinct keys, or one upload
// silently overwrites the other.
func TestGenerateKey_ReplicasCapturedTogetherGetDistinctKeys(t *testing.T) {
	u := &S3Uploader{bucket: "bucket", prefix: "profiles"}
	at := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)

	keys := map[string]string{} // key -> pod
	for _, name := range []string{"web-7d8f9c5b6d-abcde", "web-7d8f9c5b6d-fghij", "web-7d8f9c5b6d-klmno"} {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "production",
			Labels:    map[string]string{"app": "web"},
		}}
		for _, profileType := range []string{"heap", "cpu"} {
			key := u.generateKey(pod, profiler.Profile{Type: profileType, Timestamp: at})
			if other, dup := keys[key]; dup {
				t.Errorf("pods %s and %s both upload their %s profile to %s", other, name, profileType, key)
			}
			keys[key] = name
			if !strings.Contains(key, name) {
				t.Errorf("key %q does not identify pod %s", key, name)
			}
		}
	}
}
