package autoscale

import "testing"

func TestLabelsMatch(t *testing.T) {
	pool := []string{"self-hosted", "linux", "x64"}
	cases := []struct {
		job  []string
		want bool
	}{
		{[]string{"self-hosted", "linux"}, true},
		{[]string{"self-hosted", "linux", "x64"}, true},
		{[]string{"self-hosted"}, true},
		{[]string{"self-hosted", "windows"}, false}, // windows not on pool
		{[]string{"gpu"}, false},
		{nil, true}, // no requested labels -> any runner matches
		// GitHub's default self-hosted labels are capitalized. Matching must be
		// case-insensitive or autoscale never launches for a standard workflow.
		{[]string{"self-hosted", "Linux", "X64"}, true},
		{[]string{"Self-Hosted", "LINUX"}, true},
	}
	for _, c := range cases {
		if got := labelsMatch(pool, c.job); got != c.want {
			t.Errorf("labelsMatch(%v, %v) = %v, want %v", pool, c.job, got, c.want)
		}
	}
}

// TestLabelsMatchGitHubWindowsCasing pins the exact live regression: a Windows
// pool configured with lowercase labels must serve `runs-on: [self-hosted,
// Windows, X64]`, which is what GitHub reports for every standard Windows job.
func TestLabelsMatchGitHubWindowsCasing(t *testing.T) {
	pool := []string{"self-hosted", "windows", "x64"}
	job := []string{"self-hosted", "Windows", "X64"}
	if !labelsMatch(pool, job) {
		t.Fatalf("labelsMatch(%v, %v) = false, want true", pool, job)
	}
}

// TestLabelsMatchStillRejectsWrongOS guards against the fix over-matching: a
// Linux pool must not pick up a Windows job just because casing is ignored.
func TestLabelsMatchStillRejectsWrongOS(t *testing.T) {
	pool := []string{"self-hosted", "linux", "x64"}
	job := []string{"self-hosted", "Windows", "X64"}
	if labelsMatch(pool, job) {
		t.Fatalf("labelsMatch(%v, %v) = true, want false", pool, job)
	}
}
