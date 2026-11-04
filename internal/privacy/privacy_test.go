package privacy

import "testing"

func TestPersonal(t *testing.T) {
	d, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"Where is my order #4417?", "order 99812 status", "mera order kab aayega", "email me at a.b@example.com", "call 9876543210", "my otp is 123456", "pay me at test.user@okaxis"} {
		if !d.Personal(s) {
			t.Errorf("should be personal: %q", s)
		}
	}
	for _, s := range []string{"How do I reset my password?", "explain CAP theorem", "what is your refund policy"} {
		if d.Personal(s) {
			t.Errorf("should not be personal: %q", s)
		}
	}
}
