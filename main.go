package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/google/go-github/v69/github"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/util/sets"
)

func main() {
	cmd := &cobra.Command{}
	authorsToIgnore := cmd.Flags().StringArray("authors-to-ignore", nil, "Authors to ignore")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		return run(cmd.Context(), sets.New(*authorsToIgnore...))
	}

	if err := cmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, authorsToIgnore sets.Set[string]) error {
	l, err := zap.NewDevelopment()
	if err != nil {
		return fmt.Errorf("failed to get logger: %w", err)
	}
	client := github.NewClient(nil).WithAuthToken(os.Getenv("GITHUB_TOKEN"))
	result, _, err := client.Activity.ListNotifications(ctx, &github.NotificationListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list github notifications: %w", err)
	}
	l.Sugar().Infof("got %d notifications", len(result))

	var toMarkRead []string
	for _, n := range result {
		if n.Subject != nil && n.Subject.Type != nil && *n.Subject.Type == "PullRequest" {
			urlPieces := strings.Split(n.Subject.GetURL(), "/")
			numberS := urlPieces[len(urlPieces)-1]
			number, err := strconv.Atoi(numberS)
			if err != nil {
				return fmt.Errorf("failed to convert PR number %q extracted from URL %q to int: %w", numberS, n.Subject.GetURL(), err)
			}
			pr, _, err := client.PullRequests.Get(ctx, n.Repository.GetOwner().GetLogin(), n.Repository.GetName(), number)
			if err != nil {
				return fmt.Errorf("failed to fetch PR %d for repository %q: %w", number, n.Repository.GetFullName(), err)
			}
			author := strings.TrimSuffix(*pr.User.Login, "[bot]")
			if authorsToIgnore.Has(author) {
				toMarkRead = append(toMarkRead, *n.ID)
			}
		}
	}

	l.Sugar().Infof("marking %d notifications as read", len(toMarkRead))
	for _, id := range toMarkRead {
		if _, err := client.Activity.MarkThreadRead(ctx, id); err != nil {
			return fmt.Errorf("failed to mark notification %q as read: %w", id, err)
		}
	}

	return nil
}
