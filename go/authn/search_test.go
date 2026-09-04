package authn

import (
	"context"
	"errors"
	"testing"

	"github.com/vislake/speed/go/authn/internal/testutil"
)

func TestUserRepository_Search_EmailExactMatch(t *testing.T) {
	db := testutil.NewDB(t)
	repo := newTestUserRepository(t, db)
	ctx := context.Background()

	if err := repo.Create(ctx, &User{DisplayName: "Alice", Email: "alice@example.com"}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := repo.Create(ctx, &User{DisplayName: "Bob", Email: "bob@example.com"}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	got, err := repo.Search(ctx, UserSearchQuery{Email: "  Alice@Example.COM "})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(got) != 1 || got[0].DisplayName != "Alice" {
		t.Fatalf("Search() = %+v, want exactly Alice", got)
	}
}

func TestUserRepository_Search_EmailNoMatch_ReturnsEmptyNotError(t *testing.T) {
	db := testutil.NewDB(t)
	repo := newTestUserRepository(t, db)
	ctx := context.Background()

	got, err := repo.Search(ctx, UserSearchQuery{Email: "nobody@example.com"})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Search() = %+v, want none", got)
	}
}

func TestUserRepository_Search_PhoneExactMatch(t *testing.T) {
	db := testutil.NewDB(t)
	repo := newTestUserRepository(t, db)
	ctx := context.Background()

	if err := repo.Create(ctx, &User{DisplayName: "Carol", Phone: "+8613800000000"}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	got, err := repo.Search(ctx, UserSearchQuery{Phone: "+86 138 0000 0000"})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(got) != 1 || got[0].DisplayName != "Carol" {
		t.Fatalf("Search() = %+v, want exactly Carol", got)
	}
}

func TestUserRepository_Search_DisplayNamePrefix_CaseInsensitive(t *testing.T) {
	db := testutil.NewDB(t)
	repo := newTestUserRepository(t, db)
	ctx := context.Background()

	for _, name := range []string{"Alice Smith", "alicia jones", "Bob Alison", "Zed"} {
		if err := repo.Create(ctx, &User{DisplayName: name}); err != nil {
			t.Fatalf("Create(%q) error = %v", name, err)
		}
	}

	got, err := repo.Search(ctx, UserSearchQuery{DisplayNamePrefix: "ali"})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	names := make(map[string]bool, len(got))
	for _, u := range got {
		names[u.DisplayName] = true
	}
	if !names["Alice Smith"] || !names["alicia jones"] {
		t.Fatalf("Search() = %+v, want Alice Smith and alicia jones (case-insensitive prefix)", got)
	}
	if names["Bob Alison"] || names["Zed"] {
		t.Fatalf("Search() = %+v, want only names starting with \"ali\"", got)
	}
}

// TestUserRepository_Search_DisplayNamePrefix_EscapesLikeMetacharacters is
// the regression test for the bug a naive db.Where("... LIKE ?", prefix+"%")
// would have: a display name containing a literal '%' or '_' would let
// those characters act as SQL wildcards instead of being matched literally,
// so a search for "50% off" would (before the fix) also match completely
// unrelated names like "50X off". It fails on the unescaped implementation
// and passes once escapeLikePattern is applied.
func TestUserRepository_Search_DisplayNamePrefix_EscapesLikeMetacharacters(t *testing.T) {
	db := testutil.NewDB(t)
	repo := newTestUserRepository(t, db)
	ctx := context.Background()

	if err := repo.Create(ctx, &User{DisplayName: "50% off promo"}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := repo.Create(ctx, &User{DisplayName: "50X off unrelated"}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	got, err := repo.Search(ctx, UserSearchQuery{DisplayNamePrefix: "50%"})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(got) != 1 || got[0].DisplayName != "50% off promo" {
		t.Fatalf("Search() = %+v, want exactly the literal \"50%%\" match, not the '_'-style wildcard match", got)
	}
}

func TestUserRepository_Search_NoCriteria_ReturnsErrSearchCriteriaRequired(t *testing.T) {
	db := testutil.NewDB(t)
	repo := newTestUserRepository(t, db)
	ctx := context.Background()

	_, err := repo.Search(ctx, UserSearchQuery{})
	if !errors.Is(err, ErrSearchCriteriaRequired) {
		t.Fatalf("Search() error = %v, want ErrSearchCriteriaRequired", err)
	}
}

func TestUserRepository_Search_DisplayNamePrefix_LimitClamped(t *testing.T) {
	db := testutil.NewDB(t)
	repo := newTestUserRepository(t, db)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if err := repo.Create(ctx, &User{DisplayName: "dup"}); err != nil {
			t.Fatalf("Create() error = %v", err)
		}
	}

	got, err := repo.Search(ctx, UserSearchQuery{DisplayNamePrefix: "dup", Limit: 2})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Search() returned %d rows, want 2 (Limit honored)", len(got))
	}
}

func TestService_SearchUsers_DelegatesToRepository(t *testing.T) {
	db := testutil.NewDB(t)
	repo := newTestUserRepository(t, db)
	svc := &Service{users: repo}
	ctx := context.Background()

	if err := repo.Create(ctx, &User{DisplayName: "Dana", Email: "dana@example.com"}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	got, err := svc.SearchUsers(ctx, UserSearchQuery{Email: "dana@example.com"})
	if err != nil {
		t.Fatalf("SearchUsers() error = %v", err)
	}
	if len(got) != 1 || got[0].DisplayName != "Dana" {
		t.Fatalf("SearchUsers() = %+v, want exactly Dana", got)
	}
}
