package feature

import (
	"reflect"
	"testing"

	"github.com/hiylo/starburst-backend/internal/store"
)

func TestCluster(t *testing.T) {
	tests := []struct {
		name      string
		endpoints []*store.IntelEndpoint
		want      []Candidate
	}{
		{
			name: "empty input",
			want: []Candidate{},
		},
		{
			name:      "nil input",
			endpoints: nil,
			want:      []Candidate{},
		},
		{
			name: "controller path and ends clustering sorted",
			endpoints: []*store.IntelEndpoint{
				{ID: 1, Method: "GET", Path: "/banner/list", SourceFile: "com/x/app/controller/BannerController.java"},
				{ID: 2, Method: "GET", Path: "/banner/detail", SourceFile: "com/x/app/android/BannerController.java"},
				{ID: 3, Method: "GET", Path: "/order/list", SourceFile: "com/x/app/controller/OrderController.java"},
				{ID: 5, Method: "GET", Path: "/user/profile", SourceFile: "ios/NetworkService.swift"},
				{ID: 4, Method: "GET", Path: "/user/settings", SourceFile: ""},
			},
			want: []Candidate{
				{
					Name:        "Banner",
					Anchor:      "Banner",
					EndpointIDs: []int64{1, 2},
					Ends:        []string{"android", "java"},
					Confidence:  "high",
				},
				{
					Name:        "Order",
					Anchor:      "Order",
					EndpointIDs: []int64{3},
					Ends:        []string{"java"},
					Confidence:  "high",
				},
				{
					Name:        "User",
					Anchor:      "/user",
					EndpointIDs: []int64{4, 5},
					Ends:        []string{"ios", "java"},
					Confidence:  "medium",
				},
			},
		},
		{
			name: "path parameters skipped for segment grouping",
			endpoints: []*store.IntelEndpoint{
				{ID: 1, Method: "GET", Path: "/:id/banner", SourceFile: ""},
				{ID: 2, Method: "GET", Path: "/{id}/banner", SourceFile: ""},
			},
			want: []Candidate{
				{
					Name:        "Banner",
					Anchor:      "/banner",
					EndpointIDs: []int64{1, 2},
					Ends:        []string{"java"},
					Confidence:  "medium",
				},
			},
		},
		{
			name: "ends keyword case insensitive and deduplicated",
			endpoints: []*store.IntelEndpoint{
				{ID: 1, Method: "GET", Path: "/banner/list", SourceFile: "Android/BannerController.java"},
			},
			want: []Candidate{
				{
					Name:        "Banner",
					Anchor:      "Banner",
					EndpointIDs: []int64{1},
					Ends:        []string{"android", "java"},
					Confidence:  "high",
				},
			},
		},
		{
			name: "graphql operations cluster to bff end",
			endpoints: []*store.IntelEndpoint{
				{ID: 1, Method: "QUERY", Path: "activities", SourceFile: "com/x/app/bff/ActivityController.java"},
				{ID: 2, Method: "MUTATION", Path: "createActivity", SourceFile: "com/x/app/bff/ActivityController.java"},
			},
			want: []Candidate{
				{
					Name:        "Activity",
					Anchor:      "Activity",
					EndpointIDs: []int64{1, 2},
					Ends:        []string{"bff"},
					Confidence:  "high",
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Cluster(tt.endpoints)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Cluster() = %#v, want %#v", got, tt.want)
			}
		})
	}
}
