package proxycache

import "testing"

// npm と同じく、pypi / go / cargo の相対パスにも検証を掛ける。
func TestParsePathsRejectTraversal(t *testing.T) {
	for _, p := range []string{"/pypi/simple/../x", "/pypi/packages/../../x.whl"} {
		if _, _, _, _, ok := parsePyPIPath(p); ok {
			t.Errorf("parsePyPIPath(%q) を通した", p)
		}
	}
	for _, p := range []string{"/go/../x/@v/list", "/go/a/../../b/@v/list"} {
		if _, _, _, ok := parseGoPath(p); ok {
			t.Errorf("parseGoPath(%q) を通した", p)
		}
	}
	for _, p := range []string{"/cargo/index/../../x", "/cargo/crates/../x"} {
		if _, _, _, _, ok := parseCargoPath(p); ok {
			t.Errorf("parseCargoPath(%q) を通した", p)
		}
	}
}
