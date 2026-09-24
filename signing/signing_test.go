package signing

import (
	"net/url"
	"testing"
)

func TestSignAndVerify(t *testing.T) {
	signer := New("secret")
	signature := signer.Sign("/api/feeds/abc.xml")

	if !signer.Verify("/api/feeds/abc.xml", signature) {
		t.Error("valid signature rejected")
	}
	if url.QueryEscape(signature) != signature {
		t.Errorf("signature %q needs escaping", signature)
	}
	if signer.Sign("/api/feeds/abc.xml") != signature {
		t.Error("signing is not deterministic")
	}

	rejected := []struct{ path, signature string }{
		{"/api/feeds/abd.xml", signature},
		{"/api/feeds/abc.xml", ""},
		{"/api/feeds/abc.xml", signature[:len(signature)-1]},
		{"/api/feeds/abc.xml", New("other").Sign("/api/feeds/abc.xml")},
	}
	for _, r := range rejected {
		if signer.Verify(r.path, r.signature) {
			t.Errorf("Verify(%q, %q) accepted", r.path, r.signature)
		}
	}
}
