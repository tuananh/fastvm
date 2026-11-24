package names

import "testing"

func TestGenerateShape(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 20; i++ {
		name, err := Generate()
		if err != nil {
			t.Fatal(err)
		}
		if name == "" || name[0] == '-' || name[len(name)-1] == '-' {
			t.Fatalf("invalid name %q", name)
		}
		seen[name] = true
	}
	if len(seen) < 10 {
		t.Fatalf("names are not varied enough: %v", seen)
	}
}
