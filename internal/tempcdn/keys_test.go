package tempcdn

import "testing"

func TestCleanupKeyContract(t *testing.T) {
	key := "tmp/v1/0001800000000/file_1/copy-1.png"
	if n, ok := Expiry(key); !ok || n != 1800000000 {
		t.Fatal("valid expiry rejected")
	}
	if got := JobKey(key, 1800000000); got != "jobs/v1/0001800000000/f2634faf3c13fce0eb63d9c71a768cbe9405a204ddb0dde90c56fc7b52d81232.json" {
		t.Fatalf("cleanup journal contract changed: %s", got)
	}
	for _, bad := range []string{"pimg/file.png", "tmp/legacy/copy.png", "tmp/v1/1800000000/f/c.png", "tmp/v1/0000000000000/f/c.png", "tmp/v1/0001800000000/../c.png", "tmp/v1/0001800000000/f/c.png?extra"} {
		if _, ok := Expiry(bad); ok {
			t.Fatalf("unsafe key accepted: %s", bad)
		}
	}
}
