package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/go-github/v69/github"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/util/sets"
)

func main() {
	cmd := &cobra.Command{}
	authorsToIgnore := cmd.Flags().StringArray("authors-to-ignore", nil, "Authors to ignore")
	teamstoIgnore := cmd.Flags().StringArray("teams-to-ignore", nil, "Teams to ignore")
	interval := cmd.Flags().Duration("interval", 0, "If set, run continuously, repeating every interval (e.g. 5m). If unset, run once and exit.")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		return run(cmd.Context(), sets.New(*authorsToIgnore...), sets.New(*teamstoIgnore...), *interval)
	}

	if err := cmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, authorsToIgnore, teamstoIgnore sets.Set[string], interval time.Duration) error {
	l, err := zap.NewDevelopment()
	if err != nil {
		return fmt.Errorf("failed to get logger: %w", err)
	}
	client := github.NewClient(nil).WithAuthToken(os.Getenv("GITHUB_TOKEN"))

	if interval <= 0 {
		return markNotifications(ctx, l, client, authorsToIgnore, teamstoIgnore)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := markNotifications(ctx, l, client, authorsToIgnore, teamstoIgnore); err != nil {
			l.Sugar().Errorf("failed to mark notifications: %v", err)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func markNotifications(ctx context.Context, l *zap.Logger, client *github.Client, authorsToIgnore, teamstoIgnore sets.Set[string]) error {
	userResp, _, err := client.Users.Get(ctx, "")
	if err != nil {
		return fmt.Errorf("failed to get current user: %w", err)
	}
	currentUser := userResp.GetLogin()

	opts := &github.NotificationListOptions{}
	for {
		result, response, err := client.Activity.ListNotifications(ctx, opts)
		if err != nil {
			return fmt.Errorf("failed to list github notifications: %w", err)
		}
		l.Sugar().Infof("got %d notifications", len(result))

		if err := processNotifications(ctx, l, client, authorsToIgnore, teamstoIgnore, currentUser, result); err != nil {
			return fmt.Errorf("failed to process notifications: %w", err)
		}

		if response.NextPage == 0 {
			break
		}

		opts.Page = response.NextPage
	}

	return nil
}

func processNotifications(
	ctx context.Context,
	l *zap.Logger,
	client *github.Client,
	authorsToIgnore sets.Set[string],
	teamstoIgnore sets.Set[string],
	currentUser string,
	notifications []*github.Notification,
) error {
	var toMarkRead []*github.Notification
	for _, n := range notifications {
		// Always allow explicit mention
		if *n.Reason != "review_requested" {
			continue
		}
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
				toMarkRead = append(toMarkRead, n)
				continue
			}

			events, _, err := client.Issues.ListIssueEvents(ctx, n.Repository.GetOwner().GetLogin(), n.Repository.GetName(), number, &github.ListOptions{})
			if err != nil {
				return fmt.Errorf("failed to fetch events for PR %d for repository %q: %w", number, n.Repository.GetFullName(), err)
			}
			var requstedIgnoredTeam, requestedUser bool
			for _, event := range events {
				if *event.Event != "review_requested" {
					continue
				}
				if event.RequestedTeam != nil && teamstoIgnore.Has(*event.RequestedTeam.Name) {
					requstedIgnoredTeam = true
				}
				if event.RequestedReviewer != nil && *event.RequestedReviewer.Login == currentUser {
					requestedUser = true
				}
			}

			if requstedIgnoredTeam && !requestedUser {
				toMarkRead = append(toMarkRead, n)
			}
		}
	}

	l.Sugar().Infof("marking %d notifications as read", len(toMarkRead))
	for _, notification := range toMarkRead {
		if _, err := client.Activity.MarkThreadRead(ctx, *notification.ID); err != nil {
			return fmt.Errorf("failed to mark notification %q as read: %w", *notification.ID, err)
		}
		l.Sugar().Info("marked notification as read ", *notification.Subject.URL)
	}

	return nil
}
