// Package directory holds what the gateway and the executor must agree on about Forgejo, the user
// directory: which user names are accepted, and which teams are which reader roles.
package directory

import "regexp"

// UserName accepts lower-case names only. Forgejo matches names without regard to case, but the
// approval core binds the exact string, so "Alice" and "alice" would be one Forgejo user and two
// identities. Only the lower-case form is accepted, and anything else is refused before it reaches a
// URL or a session.
var UserName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,39}$`)

// TeamRoles maps Forgejo teams to reader roles, the higher first.
var TeamRoles = []struct{ Team, Role string }{{"engineers", "engineer"}, {"technicians", "technician"}}

// RoleFromGroups is the highest role among an OpenID groups claim, in which Forgejo names a team as
// "org:team". Teams of other organisations give no role.
func RoleFromGroups(org string, groups []string) string {
	member := map[string]bool{}
	for _, g := range groups {
		member[g] = true
	}
	for _, tr := range TeamRoles {
		if member[org+":"+tr.Team] {
			return tr.Role
		}
	}
	return ""
}
