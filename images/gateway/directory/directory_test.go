package directory

import "testing"

func TestRoleFromGroups(t *testing.T) {
	for _, c := range []struct {
		groups []string
		want   string
	}{
		{[]string{"maintenance", "maintenance:technicians"}, "technician"},
		{[]string{"maintenance:technicians", "maintenance:engineers"}, "engineer"},
		{[]string{"maintenance"}, ""},
		{[]string{"other:engineers", "maintenance:Engineers", "engineers"}, ""},
		{nil, ""},
	} {
		if got := RoleFromGroups("maintenance", c.groups); got != c.want {
			t.Errorf("%v: got %q, want %q", c.groups, got, c.want)
		}
	}
}

func TestUserName(t *testing.T) {
	for name, want := range map[string]bool{"alice": true, "a.b-c_9": true, "Alice": false, "": false,
		"-alice": false, "alice/../x": false, "alice\n": false} {
		if got := UserName.MatchString(name); got != want {
			t.Errorf("%q: got %v, want %v", name, got, want)
		}
	}
}
