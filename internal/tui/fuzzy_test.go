package tui

import (
	"reflect"
	"testing"

	"github.com/jmserra/jsq/internal/db"
)

// TestSidebarRanking: the list filter keeps every match but orders them exact,
// prefix, word prefix, substring, then letters-in-order — shorter first within a
// tier — and a `%` falls back to the grid's LIKE filter in list order.
func TestSidebarRanking(t *testing.T) {
	var s sidebar
	s.setTables([]db.Table{
		{Name: "user_roles"}, {Name: "audit_user_log"}, {Name: "superusers"},
		{Name: "users"}, {Name: "uploads_queue_store"}, {Name: "orders"},
	})
	names := func(q string) []string {
		s.filter.val = q
		s.cursor, s.off = 0, 0
		s.rebuildVisible()
		var out []string
		for _, i := range s.visible {
			out = append(out, s.tables[i].Name)
		}
		return out
	}

	got := names("user")
	want := []string{"users", "user_roles", "audit_user_log", "superusers", "uploads_queue_store"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("/user = %v, want %v", got, want)
	}
	// "usr" is a substring of nothing: subsequence hits, tightest span first.
	got = names("usr")
	want = []string{"users", "user_roles", "superusers", "audit_user_log", "uploads_queue_store"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("/usr = %v, want %v", got, want)
	}
	if got = names("zzz"); len(got) != 0 {
		t.Fatalf("/zzz = %v, want nothing", got)
	}
	// A typed % is a LIKE pattern: anchored, list order, no ranking.
	got = names("%user%")
	want = []string{"user_roles", "audit_user_log", "superusers", "users"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("/%%user%% = %v, want %v", got, want)
	}
}

// TestRankLabelSchemaExact: `users` is an exact hit on public.users, above a
// table that merely starts with it.
func TestRankLabelSchemaExact(t *testing.T) {
	a, _ := rankLabel("public.users", "users")
	b, _ := rankLabel("users_archive", "users")
	if !a.less(b) {
		t.Fatalf("public.users (%+v) should outrank users_archive (%+v)", a, b)
	}
}
