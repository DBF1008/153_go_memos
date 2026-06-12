package test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	apiv1 "github.com/usememos/memos/proto/gen/api/v1"
	"github.com/usememos/memos/store"
)

func TestListUsersPagination(t *testing.T) {
	ctx := context.Background()

	t.Run("default page size", func(t *testing.T) {
		ts := NewTestService(t)
		defer ts.Cleanup()

		hostUser, err := ts.CreateHostUser(ctx, "admin")
		require.NoError(t, err)
		hostCtx := ts.CreateUserContext(ctx, hostUser.ID)

		// Create 15 regular users (total 16 with admin).
		for i := 0; i < 15; i++ {
			_, err := ts.CreateRegularUser(ctx, fmt.Sprintf("user%02d", i))
			require.NoError(t, err)
		}

		// Default page size is 10.
		resp, err := ts.Service.ListUsers(hostCtx, &apiv1.ListUsersRequest{})
		require.NoError(t, err)
		require.Equal(t, int32(16), resp.TotalSize)
		require.Equal(t, 10, len(resp.Users))
		require.NotEmpty(t, resp.NextPageToken)
	})

	t.Run("custom page size", func(t *testing.T) {
		ts := NewTestService(t)
		defer ts.Cleanup()

		hostUser, err := ts.CreateHostUser(ctx, "admin")
		require.NoError(t, err)
		hostCtx := ts.CreateUserContext(ctx, hostUser.ID)

		for i := 0; i < 4; i++ {
			_, err := ts.CreateRegularUser(ctx, fmt.Sprintf("user%02d", i))
			require.NoError(t, err)
		}

		// Request page_size=2.
		resp, err := ts.Service.ListUsers(hostCtx, &apiv1.ListUsersRequest{PageSize: 2})
		require.NoError(t, err)
		require.Equal(t, int32(5), resp.TotalSize)
		require.Equal(t, 2, len(resp.Users))
		require.NotEmpty(t, resp.NextPageToken)
	})

	t.Run("page token retrieves next page", func(t *testing.T) {
		ts := NewTestService(t)
		defer ts.Cleanup()

		hostUser, err := ts.CreateHostUser(ctx, "admin")
		require.NoError(t, err)
		hostCtx := ts.CreateUserContext(ctx, hostUser.ID)

		for i := 0; i < 4; i++ {
			_, err := ts.CreateRegularUser(ctx, fmt.Sprintf("user%02d", i))
			require.NoError(t, err)
		}

		// Page 1.
		page1, err := ts.Service.ListUsers(hostCtx, &apiv1.ListUsersRequest{PageSize: 2})
		require.NoError(t, err)
		require.Equal(t, 2, len(page1.Users))
		require.NotEmpty(t, page1.NextPageToken)

		// Page 2.
		page2, err := ts.Service.ListUsers(hostCtx, &apiv1.ListUsersRequest{PageToken: page1.NextPageToken})
		require.NoError(t, err)
		require.Equal(t, 2, len(page2.Users))
		require.NotEmpty(t, page2.NextPageToken)

		// Pages should return different users.
		require.NotEqual(t, page1.Users[0].Username, page2.Users[0].Username)
		require.NotEqual(t, page1.Users[1].Username, page2.Users[1].Username)

		// Page 3.
		page3, err := ts.Service.ListUsers(hostCtx, &apiv1.ListUsersRequest{PageToken: page2.NextPageToken})
		require.NoError(t, err)
		require.Equal(t, 1, len(page3.Users))
		require.Empty(t, page3.NextPageToken) // no more pages
	})

	t.Run("full pagination walk", func(t *testing.T) {
		ts := NewTestService(t)
		defer ts.Cleanup()

		hostUser, err := ts.CreateHostUser(ctx, "admin")
		require.NoError(t, err)
		hostCtx := ts.CreateUserContext(ctx, hostUser.ID)

		for i := 0; i < 6; i++ {
			_, err := ts.CreateRegularUser(ctx, fmt.Sprintf("walk%02d", i))
			require.NoError(t, err)
		}

		// Walk through all pages with page_size=3, collect all usernames.
		var allUsernames []string
		pageToken := ""
		pages := 0
		for {
			resp, err := ts.Service.ListUsers(hostCtx, &apiv1.ListUsersRequest{
				PageSize:  3,
				PageToken: pageToken,
			})
			require.NoError(t, err)
			for _, u := range resp.Users {
				allUsernames = append(allUsernames, u.Username)
			}
			pages++
			if resp.NextPageToken == "" {
				break
			}
			pageToken = resp.NextPageToken
		}

		require.Equal(t, 7, len(allUsernames))
		require.Equal(t, 3, pages) // 3+3+1

		// No duplicates.
		seen := make(map[string]bool)
		for _, name := range allUsernames {
			require.False(t, seen[name], "duplicate user: %s", name)
			seen[name] = true
		}
	})

	t.Run("page size larger than total", func(t *testing.T) {
		ts := NewTestService(t)
		defer ts.Cleanup()

		hostUser, err := ts.CreateHostUser(ctx, "admin")
		require.NoError(t, err)
		hostCtx := ts.CreateUserContext(ctx, hostUser.ID)

		for i := 0; i < 2; i++ {
			_, err := ts.CreateRegularUser(ctx, fmt.Sprintf("user%02d", i))
			require.NoError(t, err)
		}

		resp, err := ts.Service.ListUsers(hostCtx, &apiv1.ListUsersRequest{PageSize: 100})
		require.NoError(t, err)
		require.Equal(t, int32(3), resp.TotalSize)
		require.Equal(t, 3, len(resp.Users))
		require.Empty(t, resp.NextPageToken)
	})
}

func TestListUsersShowDeleted(t *testing.T) {
	ctx := context.Background()

	t.Run("show_deleted false excludes archived users", func(t *testing.T) {
		ts := NewTestService(t)
		defer ts.Cleanup()

		hostUser, err := ts.CreateHostUser(ctx, "admin")
		require.NoError(t, err)
		hostCtx := ts.CreateUserContext(ctx, hostUser.ID)

		user1, err := ts.CreateRegularUser(ctx, "active1")
		require.NoError(t, err)
		_, err = ts.CreateRegularUser(ctx, "active2")
		require.NoError(t, err)

		// Archive user1.
		archived := store.Archived
		_, err = ts.Store.UpdateUser(ctx, &store.UpdateUser{
			ID:        user1.ID,
			RowStatus: &archived,
		})
		require.NoError(t, err)

		// Default: show_deleted=false → only NORMAL users.
		resp, err := ts.Service.ListUsers(hostCtx, &apiv1.ListUsersRequest{})
		require.NoError(t, err)
		require.Equal(t, int32(3), resp.TotalSize) // admin + active2 + (NOT archived)
		require.Equal(t, 3, len(resp.Users))
		for _, u := range resp.Users {
			require.NotEqual(t, "active1", u.Username)
		}
	})

	t.Run("show_deleted true includes archived users", func(t *testing.T) {
		ts := NewTestService(t)
		defer ts.Cleanup()

		hostUser, err := ts.CreateHostUser(ctx, "admin")
		require.NoError(t, err)
		hostCtx := ts.CreateUserContext(ctx, hostUser.ID)

		user1, err := ts.CreateRegularUser(ctx, "active1")
		require.NoError(t, err)
		_, err = ts.CreateRegularUser(ctx, "active2")
		require.NoError(t, err)

		// Archive user1.
		archived := store.Archived
		_, err = ts.Store.UpdateUser(ctx, &store.UpdateUser{
			ID:        user1.ID,
			RowStatus: &archived,
		})
		require.NoError(t, err)

		// show_deleted=true → all users including archived.
		resp, err := ts.Service.ListUsers(hostCtx, &apiv1.ListUsersRequest{ShowDeleted: true})
		require.NoError(t, err)
		require.Equal(t, int32(4), resp.TotalSize) // admin + active1(archived) + active2
		require.Equal(t, 4, len(resp.Users))
	})

	t.Run("show_deleted with pagination", func(t *testing.T) {
		ts := NewTestService(t)
		defer ts.Cleanup()

		hostUser, err := ts.CreateHostUser(ctx, "admin")
		require.NoError(t, err)
		hostCtx := ts.CreateUserContext(ctx, hostUser.ID)

		// Create 5 users and archive 2 of them.
		var archivedIDs []int32
		for i := 0; i < 5; i++ {
			u, err := ts.CreateRegularUser(ctx, fmt.Sprintf("sduser%02d", i))
			require.NoError(t, err)
			if i%2 == 0 {
				archived := store.Archived
				_, err = ts.Store.UpdateUser(ctx, &store.UpdateUser{
					ID:        u.ID,
					RowStatus: &archived,
				})
				require.NoError(t, err)
				archivedIDs = append(archivedIDs, u.ID)
			}
		}

		// show_deleted=false, page_size=2 → expect 3 normal users (admin + 2 non-archived).
		var normalNames []string
		pageToken := ""
		for {
			resp, err := ts.Service.ListUsers(hostCtx, &apiv1.ListUsersRequest{
				PageSize:  2,
				PageToken: pageToken,
			})
			require.NoError(t, err)
			require.Equal(t, int32(3), resp.TotalSize)
			for _, u := range resp.Users {
				normalNames = append(normalNames, u.Username)
			}
			if resp.NextPageToken == "" {
				break
			}
			pageToken = resp.NextPageToken
		}
		require.Equal(t, 3, len(normalNames))

		// show_deleted=true, page_size=2 → expect 6 users (admin + 5 created).
		var allNames []string
		pageToken = ""
		for {
			resp, err := ts.Service.ListUsers(hostCtx, &apiv1.ListUsersRequest{
				PageSize:    2,
				PageToken:   pageToken,
				ShowDeleted: true,
			})
			require.NoError(t, err)
			require.Equal(t, int32(6), resp.TotalSize)
			for _, u := range resp.Users {
				allNames = append(allNames, u.Username)
			}
			if resp.NextPageToken == "" {
				break
			}
			pageToken = resp.NextPageToken
		}
		require.Equal(t, 6, len(allNames))
	})
}

func TestListUsersTotalSizeConsistency(t *testing.T) {
	ctx := context.Background()

	t.Run("total_size reflects filter", func(t *testing.T) {
		ts := NewTestService(t)
		defer ts.Cleanup()

		hostUser, err := ts.CreateHostUser(ctx, "admin")
		require.NoError(t, err)
		hostCtx := ts.CreateUserContext(ctx, hostUser.ID)

		for i := 0; i < 3; i++ {
			_, err := ts.CreateRegularUser(ctx, fmt.Sprintf("filtuser%02d", i))
			require.NoError(t, err)
		}

		// No filter: total_size = 4 (admin + 3 users).
		resp, err := ts.Service.ListUsers(hostCtx, &apiv1.ListUsersRequest{PageSize: 100})
		require.NoError(t, err)
		require.Equal(t, int32(4), resp.TotalSize)

		// Filter by username: total_size = 1.
		resp, err = ts.Service.ListUsers(hostCtx, &apiv1.ListUsersRequest{
			PageSize: 100,
			Filter:   `username == "filtuser01"`,
		})
		require.NoError(t, err)
		require.Equal(t, int32(1), resp.TotalSize)
		require.Equal(t, 1, len(resp.Users))
		require.Equal(t, "filtuser01", resp.Users[0].Username)
	})

	t.Run("total_size is consistent across pages", func(t *testing.T) {
		ts := NewTestService(t)
		defer ts.Cleanup()

		hostUser, err := ts.CreateHostUser(ctx, "admin")
		require.NoError(t, err)
		hostCtx := ts.CreateUserContext(ctx, hostUser.ID)

		for i := 0; i < 7; i++ {
			_, err := ts.CreateRegularUser(ctx, fmt.Sprintf("pageuser%02d", i))
			require.NoError(t, err)
		}

		// Total = 8 (admin + 7).
		page1, err := ts.Service.ListUsers(hostCtx, &apiv1.ListUsersRequest{PageSize: 3})
		require.NoError(t, err)
		require.Equal(t, int32(8), page1.TotalSize)

		page2, err := ts.Service.ListUsers(hostCtx, &apiv1.ListUsersRequest{PageToken: page1.NextPageToken})
		require.NoError(t, err)
		require.Equal(t, int32(8), page2.TotalSize) // total_size stays the same

		page3, err := ts.Service.ListUsers(hostCtx, &apiv1.ListUsersRequest{PageToken: page2.NextPageToken})
		require.NoError(t, err)
		require.Equal(t, int32(8), page3.TotalSize)
	})
}

func TestListUsersPermissionDenied(t *testing.T) {
	ctx := context.Background()

	t.Run("non-admin cannot list users", func(t *testing.T) {
		ts := NewTestService(t)
		defer ts.Cleanup()

		hostUser, err := ts.CreateHostUser(ctx, "admin")
		require.NoError(t, err)
		_ = hostUser

		regularUser, err := ts.CreateRegularUser(ctx, "regular")
		require.NoError(t, err)
		regularCtx := ts.CreateUserContext(ctx, regularUser.ID)

		_, err = ts.Service.ListUsers(regularCtx, &apiv1.ListUsersRequest{})
		require.Error(t, err)
		require.Contains(t, err.Error(), "permission denied")
	})
}
