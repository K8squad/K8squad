package auth

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const mappingJSON = `{
  "platform-admins": "admin",
  "k8s-devs": {"project": "my-project", "role": "contributor"},
  "k8s-viewers": {"project": "my-project", "role": "viewer"},
  "k8s-maint": {"project": "other-project", "role": "maintainer"}
}`

func TestParseGroupMapping_Shapes(t *testing.T) {
	gm, err := ParseGroupMapping(mappingJSON)
	require.NoError(t, err)
	require.Len(t, gm, 4)
	assert.True(t, gm["platform-admins"].Admin)
	assert.Equal(t, "contributor", gm["k8s-devs"].Membership.Role)
}

func TestParseGroupMapping_EmptyIsNil(t *testing.T) {
	gm, err := ParseGroupMapping("  ")
	require.NoError(t, err)
	assert.Nil(t, gm)
}

func TestParseGroupMapping_FailClosed(t *testing.T) {
	for name, raw := range map[string]string{
		"not json":         `{"a":`,
		"bad string value": `{"g": "user"}`,
		"bad role":         `{"g": {"project": "p", "role": "owner"}}`,
		"missing project":  `{"g": {"role": "viewer"}}`,
	} {
		_, err := ParseGroupMapping(raw)
		assert.Error(t, err, name)
	}
}

func TestResolve_UnmappedIgnored(t *testing.T) {
	gm, err := ParseGroupMapping(mappingJSON)
	require.NoError(t, err)
	out := gm.Resolve([]string{"totally-unknown-group", "another"})
	assert.Empty(t, out.GlobalRole)
	assert.Empty(t, out.Memberships, "unmapped groups grant nothing (15.9)")
}

func TestResolve_AdminAndMembership(t *testing.T) {
	gm, err := ParseGroupMapping(mappingJSON)
	require.NoError(t, err)

	out := gm.Resolve([]string{"k8s-devs"})
	assert.Equal(t, RoleUser, out.GlobalRole)
	require.Len(t, out.Memberships, 1)
	assert.Equal(t, ProjectMembership{Project: "my-project", Role: "contributor"}, out.Memberships[0])

	out = gm.Resolve([]string{"platform-admins"})
	assert.Equal(t, RoleAdmin, out.GlobalRole)
}

func TestResolve_ConlictPromotesAdmin(t *testing.T) {
	gm, err := ParseGroupMapping(mappingJSON)
	require.NoError(t, err)
	out := gm.Resolve([]string{"platform-admins", "k8s-devs"})
	assert.Equal(t, RoleAdmin, out.GlobalRole, "admin wins over project memberships (15.9 conflict rule)")
	require.Len(t, out.Memberships, 1)
}

func TestResolve_StrongestRolePerProject(t *testing.T) {
	gm, err := ParseGroupMapping(mappingJSON)
	require.NoError(t, err)
	out := gm.Resolve([]string{"k8s-devs", "k8s-viewers"})
	require.Len(t, out.Memberships, 1)
	assert.Equal(t, "contributor", out.Memberships[0].Role, "strongest role per project wins")

	out = gm.Resolve([]string{"k8s-devs", "k8s-maint"})
	require.Len(t, out.Memberships, 2)
	assert.Equal(t, "maintainer", out.Memberships[1].Role, "projects sorted deterministically")
}
