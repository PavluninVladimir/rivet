package ops

import "testing"

func TestLocalFailureStructuredIdentity(t *testing.T) {
	a, ok := LocalPublishFingerprint("env", "publish", "build", "recipe_not_detected")
	if !ok {
		t.Fatal("not recognized")
	}
	b, _ := LocalPublishFingerprint("env", "publish", "build", "recipe_not_detected")
	if a != b {
		t.Fatal("unstable")
	}
	for _, v := range [][4]string{{"other", "publish", "build", "recipe_not_detected"}, {"env", "publish", "start", "recipe_not_detected"}, {"env", "publish", "build", "port_conflict"}} {
		b, ok := LocalPublishFingerprint(v[0], v[1], v[2], v[3])
		if !ok || a == b {
			t.Fatal("identity collision", v)
		}
	}
	for _, v := range [][4]string{{"env", "reconcile", "build", "recipe_not_detected"}, {"env", "publish", "build", "build_failed"}, {"env", "publish", "build", "unknown"}, {"env", "publish", "", "recipe_not_detected"}} {
		if _, ok := LocalPublishFingerprint(v[0], v[1], v[2], v[3]); ok {
			t.Fatal("generic failure collapsed", v)
		}
	}
	if Fingerprint("env", SourceRelease, "Cloning repo") == Fingerprint("env", SourceRelease, "Docker version 29.8.2") {
		t.Fatal("fixture no longer reproduces legacy mismatch")
	}
}
