package test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	v1pb "github.com/usememos/memos/proto/gen/api/v1"
	"github.com/usememos/memos/store"
)

// listAllUsersByPaging walks every page of ListUsers using the returned
// next_page_token and returns the usernames in the order they were served along
// with the total_size reported on each page. It guards against a broken token
// that never terminates.
func listAllUsersByPaging(ctx context.Context, t *testing.T, ts *TestService, pageSize int32, showDeleted bool) (usernames []string, totals []int32) {
	t.Helper()

	pageToken := ""
	for iteration := 0; ; iteration++ {
		require.Less(t, iteration, 1000, "ListUsers pagination did not terminate; page token is not advancing")

		resp, err := ts.Service.ListUsers(ctx, &v1pb.ListUsersRequest{
			PageSize:    pageSize,
			PageToken:   pageToken,
			ShowDeleted: showDeleted,
		})
		require.NoError(t, err)
		require.LessOrEqual(t, len(resp.Users), int(pageSize), "a page returned more users than page_size")

		totals = append(totals, resp.TotalSize)
		for _, user := range resp.Users {
			usernames = append(usernames, user.Username)
		}

		if resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
	}
	return usernames, totals
}

// TestListUsersPaginationLimitsTokenAndTotalSize verifies that page_size limits
// each page, page_token advances through the full result set without duplicates
// or gaps, ordering is stable (paged results match the full single-page order),
// and total_size reports the full filtered count on every page.
func TestListUsersPaginationLimitsTokenAndTotalSize(t *testing.T) {
	t.Parallel()

	ts := NewTestService(t)
	defer ts.Cleanup()

	ctx := context.Background()
	admin, err := ts.CreateHostUser(ctx, "list-admin")
	require.NoError(t, err)
	adminCtx := ts.CreateUserContext(ctx, admin.ID)

	const regularUsers = 5
	for i := 1; i <= regularUsers; i++ {
		_, err := ts.CreateRegularUser(ctx, fmt.Sprintf("list-user-%d", i))
		require.NoError(t, err)
	}
	// Admin is also a NORMAL user, so it is included in the listing.
	totalUsers := regularUsers + 1

	// Reference ordering: a single page large enough to hold everyone.
	full, err := ts.Service.ListUsers(adminCtx, &v1pb.ListUsersRequest{PageSize: 100})
	require.NoError(t, err)
	require.Len(t, full.Users, totalUsers)
	require.Equal(t, int32(totalUsers), full.TotalSize)
	require.Empty(t, full.NextPageToken)

	reference := make([]string, 0, len(full.Users))
	for _, user := range full.Users {
		reference = append(reference, user.Username)
	}

	// First page with a small page_size must be limited and expose a token.
	firstPage, err := ts.Service.ListUsers(adminCtx, &v1pb.ListUsersRequest{PageSize: 2})
	require.NoError(t, err)
	require.Len(t, firstPage.Users, 2, "page_size was not honored")
	require.NotEmpty(t, firstPage.NextPageToken, "next_page_token missing while more pages remain")
	require.Equal(t, int32(totalUsers), firstPage.TotalSize, "total_size should reflect the full set, not the page")

	// Walking all pages with page_size=2 must reproduce the reference ordering
	// exactly: same elements, same order, no duplicates, no gaps.
	paged, totals := listAllUsersByPaging(adminCtx, t, ts, 2, false)
	require.Equal(t, reference, paged, "paged traversal diverged from the full ordering (unstable or lossy pagination)")

	unique := map[string]struct{}{}
	for _, name := range paged {
		unique[name] = struct{}{}
	}
	require.Len(t, unique, totalUsers, "pagination produced duplicate or missing users")

	for _, total := range totals {
		require.Equal(t, int32(totalUsers), total, "total_size must be constant across pages")
	}
}

// TestListUsersPaginationIsDeterministicAcrossRuns guards specifically against
// the unstable-ordering bug: repeating the same paged traversal must yield the
// identical sequence even when users share the same creation timestamp.
func TestListUsersPaginationIsDeterministicAcrossRuns(t *testing.T) {
	t.Parallel()

	ts := NewTestService(t)
	defer ts.Cleanup()

	ctx := context.Background()
	admin, err := ts.CreateHostUser(ctx, "stable-admin")
	require.NoError(t, err)
	adminCtx := ts.CreateUserContext(ctx, admin.ID)

	for i := 1; i <= 8; i++ {
		_, err := ts.CreateRegularUser(ctx, fmt.Sprintf("stable-user-%d", i))
		require.NoError(t, err)
	}

	first, _ := listAllUsersByPaging(adminCtx, t, ts, 3, false)
	second, _ := listAllUsersByPaging(adminCtx, t, ts, 3, false)
	require.Equal(t, first, second, "paged ordering changed between identical runs")
	require.Len(t, first, 9) // admin + 8 regular users
}

// TestListUsersShowDeletedControlsArchivedVisibility verifies that show_deleted
// toggles whether archived (deleted-state) accounts appear, and that total_size
// matches the corresponding visible set.
func TestListUsersShowDeletedControlsArchivedVisibility(t *testing.T) {
	t.Parallel()

	ts := NewTestService(t)
	defer ts.Cleanup()

	ctx := context.Background()
	admin, err := ts.CreateHostUser(ctx, "del-admin")
	require.NoError(t, err)
	adminCtx := ts.CreateUserContext(ctx, admin.ID)

	for i := 1; i <= 2; i++ {
		_, err := ts.CreateRegularUser(ctx, fmt.Sprintf("del-normal-%d", i))
		require.NoError(t, err)
	}

	archivedUsernames := map[string]struct{}{}
	archived := store.Archived
	for i := 1; i <= 2; i++ {
		username := fmt.Sprintf("del-archived-%d", i)
		user, err := ts.CreateRegularUser(ctx, username)
		require.NoError(t, err)
		_, err = ts.Store.UpdateUser(ctx, &store.UpdateUser{ID: user.ID, RowStatus: &archived})
		require.NoError(t, err)
		archivedUsernames[username] = struct{}{}
	}

	const normalCount = 3 // admin + 2 normal regular users
	const allCount = 5    // + 2 archived

	// show_deleted = false: archived accounts must be hidden.
	visible, totals := listAllUsersByPaging(adminCtx, t, ts, 100, false)
	require.Len(t, visible, normalCount)
	for _, name := range visible {
		_, isArchived := archivedUsernames[name]
		require.False(t, isArchived, "archived user %q leaked into show_deleted=false response", name)
	}
	for _, total := range totals {
		require.Equal(t, int32(normalCount), total)
	}

	// show_deleted = true: archived accounts must be included.
	withDeleted, totalsWithDeleted := listAllUsersByPaging(adminCtx, t, ts, 100, true)
	require.Len(t, withDeleted, allCount)
	seen := map[string]struct{}{}
	for _, name := range withDeleted {
		seen[name] = struct{}{}
	}
	for name := range archivedUsernames {
		_, ok := seen[name]
		require.True(t, ok, "archived user %q missing from show_deleted=true response", name)
	}
	for _, total := range totalsWithDeleted {
		require.Equal(t, int32(allCount), total)
	}
}
