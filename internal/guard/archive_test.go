package guard

import (
	"path/filepath"
	"testing"

	"github.com/celaldrg/undoguard/internal/policy"
)

func TestArchiveExtraction(t *testing.T) {
	w := newWorld(t)
	for _, c := range []struct {
		cmd  string
		want policy.Action
		// snap is the directory snapshotted recursively, relative to the
		// project, or "" for none.
		snap string
	}{
		{"tar -xf a.tar -C ~", policy.Deny, ""},
		{"tar xzf a.tgz -C ~/important", policy.Deny, ""},
		{"tar -C ~ -xzf a.tgz", policy.Deny, ""},
		{"unzip -o x.zip -d ~", policy.Deny, ""},
		{"tar -xf a.tar -C src", policy.Allow, "src"},
		{"tar --extract --file=a.tar --directory=build", policy.Allow, "build"},
		{"tar -C build -xf a.tar -C out", policy.Allow, "build/out"},
		{"unzip -o x.zip -d src", policy.Allow, "src"},
		{"tar xf a.tar", policy.Ask, "."},
		// Nothing existing is replaced by these.
		{"tar -czf out.tgz src", policy.Allow, ""},
		{"tar -tf a.tar", policy.Allow, ""},
		{"tar -xkf a.tar -C ~", policy.Allow, ""},
		{"tar -xOf a.tar -C ~", policy.Allow, ""},
		{"unzip x.zip -d ~", policy.Allow, ""},
		{"unzip -n x.zip -d ~", policy.Allow, ""},
		{"unzip -l x.zip", policy.Allow, ""},
	} {
		d := w.check(t, c.cmd)
		if d.Action != c.want {
			t.Errorf("%q: %s", c.cmd, describe(d))
			continue
		}
		if c.snap == "" {
			if len(d.Snapshot) != 0 && c.want != policy.Deny {
				t.Errorf("%q: unexpected snapshot %+v", c.cmd, d.Snapshot)
			}
			continue
		}
		want := filepath.Join(w.proj, c.snap)
		if len(d.Snapshot) != 1 || d.Snapshot[0].Path != want || !d.Snapshot[0].Recursive {
			t.Errorf("%q: snapshot %+v, want %s recursively", c.cmd, d.Snapshot, want)
		}
	}
}
