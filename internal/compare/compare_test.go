package compare

import (
	"testing"

	"dic/internal/registry"
)

func TestImagesPair(t *testing.T) {
	a := []registry.Layer{{Digest: "sha256:1", Size: 100}, {Digest: "sha256:2", Size: 200}, {Digest: "sha256:3", Size: 50}}
	b := []registry.Layer{{Digest: "sha256:2", Size: 200}, {Digest: "sha256:3", Size: 50}, {Digest: "sha256:4", Size: 999}}

	c := Images([]string{"a", "b"}, [][]registry.Layer{a, b})

	if c.Images[0].Total != 350 {
		t.Errorf("A.Total = %d, want 350", c.Images[0].Total)
	}
	if c.Images[1].Total != 1249 {
		t.Errorf("B.Total = %d, want 1249", c.Images[1].Total)
	}
	if c.Shared != 250 { // sha256:2 + sha256:3
		t.Errorf("Shared = %d, want 250", c.Shared)
	}
	if c.NumShared != 2 {
		t.Errorf("NumShared = %d, want 2", c.NumShared)
	}
	// shared / min(350,1249) = 250/350 = 71.4%
	if got := c.SharedPct(); got < 71 || got > 72 {
		t.Errorf("SharedPct = %.2f, want ~71.4", got)
	}
	// Per-image shared/unique: shared digests are 2 & 3 (250); A unique = 1 (100); B unique = 4 (999).
	if c.Images[0].Shared != 250 || c.Images[0].Unique != 100 {
		t.Errorf("A shared/unique = %d/%d, want 250/100", c.Images[0].Shared, c.Images[0].Unique)
	}
	if c.Images[1].Shared != 250 || c.Images[1].Unique != 999 {
		t.Errorf("B shared/unique = %d/%d, want 250/999", c.Images[1].Shared, c.Images[1].Unique)
	}
	// AvgSharedPct = mean(250/350, 250/1249) = mean(71.4%, 20.0%) = 45.7%
	if got := c.AvgSharedPct(); got < 45 || got > 46 {
		t.Errorf("AvgSharedPct = %.2f, want ~45.7", got)
	}
	// PooledSharedPct = (250+250)/(350+1249) = 500/1599 = 31.3%
	if got := c.PooledSharedPct(); got < 31 || got > 32 {
		t.Errorf("PooledSharedPct = %.2f, want ~31.3", got)
	}
	// AvgSharedSize = (250+250)/2 = 250
	if got := c.AvgSharedSize(); got != 250 {
		t.Errorf("AvgSharedSize = %d, want 250", got)
	}
}

func TestImagesMany(t *testing.T) {
	// A layer is "shared" only if present in every image.
	a := []registry.Layer{{Digest: "sha256:base", Size: 100}, {Digest: "sha256:1", Size: 10}}
	b := []registry.Layer{{Digest: "sha256:base", Size: 100}, {Digest: "sha256:2", Size: 20}}
	c := []registry.Layer{{Digest: "sha256:base", Size: 100}, {Digest: "sha256:3", Size: 30}}

	r := Images([]string{"a", "b", "c"}, [][]registry.Layer{a, b, c})
	if r.Shared != 100 || r.NumShared != 1 {
		t.Errorf("3-way shared = %d (%d layers), want 100 (1)", r.Shared, r.NumShared)
	}
	// smallest total is 110 -> 100/110 = 90.9%
	if got := r.SharedPct(); got < 90 || got > 92 {
		t.Errorf("SharedPct = %.2f, want ~90.9", got)
	}
	// Per-image: base is shared (100); the other layer is unique (10/20/30).
	for i, wantUnique := range []int64{10, 20, 30} {
		if r.Images[i].Shared != 100 || r.Images[i].Unique != wantUnique {
			t.Errorf("image %d shared/unique = %d/%d, want 100/%d", i, r.Images[i].Shared, r.Images[i].Unique, wantUnique)
		}
	}
}

func TestImagesIdentical(t *testing.T) {
	a := []registry.Layer{{Digest: "sha256:1", Size: 100}, {Digest: "sha256:2", Size: 200}}
	c := Images([]string{"a", "a"}, [][]registry.Layer{a, a})
	if c.Shared != 300 || c.SharedPct() != 100 {
		t.Errorf("identical compare: shared=%d pct=%.1f, want 300/100", c.Shared, c.SharedPct())
	}
}

func TestLayerDedup(t *testing.T) {
	// A repeated digest within one image counts once toward its total.
	a := []registry.Layer{{Digest: "sha256:1", Size: 100}, {Digest: "sha256:1", Size: 100}, {Digest: "sha256:2", Size: 50}}
	m := layerSizes(a)
	if sumSizes(m) != 150 {
		t.Errorf("deduped total = %d, want 150", sumSizes(m))
	}
}

func TestHumanSize(t *testing.T) {
	cases := map[int64]string{
		512:        "512 B",
		1000:       "1.0 kB",
		1500:       "1.5 kB",
		180400000:  "180.4 MB",
		2000000000: "2.0 GB",
	}
	for in, want := range cases {
		if got := HumanSize(in); got != want {
			t.Errorf("HumanSize(%d) = %q, want %q", in, got, want)
		}
	}
}
