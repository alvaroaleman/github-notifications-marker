package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/go-github/v69/github"
	"github.com/gregjones/httpcache"
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
		authorsToIgnoreSet := make(sets.Set[string], len(*authorsToIgnore))
		for _, author := range *authorsToIgnore {
			authorsToIgnoreSet.Insert(strings.ToLower(author))
		}
		teamstoIgnoreSet := make(sets.Set[string], len(*teamstoIgnore))
		for _, team := range *teamstoIgnore {
			teamstoIgnoreSet.Insert(strings.ToLower(team))
		}
		return run(cmd.Context(), authorsToIgnoreSet, teamstoIgnoreSet, *interval)
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
	client := github.NewClient(httpcache.NewMemoryCacheTransport().Client()).WithAuthToken(os.Getenv("GITHUB_TOKEN"))

	// Cache successful user lookups, but retry failures on the next poll.
	var currentUser string
	getUser := func() (string, error) {
		if currentUser == "" {
			user, _, err := client.Users.Get(ctx, "")
			if err != nil {
				return "", fmt.Errorf("failed to get current user: %w", err)
			}
			currentUser = user.GetLogin()
		}
		return currentUser, nil
	}

	if interval <= 0 {
		return markNotifications(ctx, l, client, authorsToIgnore, teamstoIgnore, getUser)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := markNotifications(ctx, l, client, authorsToIgnore, teamstoIgnore, getUser); err != nil {
			l.Sugar().Errorf("failed to mark notifications: %v", err)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func markNotifications(ctx context.Context, l *zap.Logger, client *github.Client, authorsToIgnore, teamstoIgnore sets.Set[string], getUser func() (string, error)) error {
	currentUser, err := getUser()
	if err != nil {
		return err
	}

	opts := &github.NotificationListOptions{ListOptions: github.ListOptions{PerPage: 100}}
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

func teamMatches(teamsToIgnore sets.Set[string], team *github.Team) bool {
	if team == nil {
		return false
	}
	for _, candidate := range []string{team.GetName(), team.GetSlug()} {
		if candidate == "" {
			continue
		}
		if teamsToIgnore.Has(strings.ToLower(candidate)) {
			return true
		}
	}
	return false
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
	var toDismiss []*github.Notification
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
				toDismiss = append(toDismiss, n)
				continue
			}

			var requestedIgnoredTeam, requestedUser bool
			for _, team := range pr.RequestedTeams {
				if teamMatches(teamstoIgnore, team) {
					requestedIgnoredTeam = true
					break
				}
			}
			for _, reviewer := range pr.RequestedReviewers {
				if strings.EqualFold(reviewer.GetLogin(), currentUser) {
					requestedUser = true
					break
				}
			}

			if requestedIgnoredTeam && !requestedUser {
				toDismiss = append(toDismiss, n)
			}
		}
	}

	l.Sugar().Infof("unsubscribing from %d notifications and marking them as read", len(toDismiss))
	for _, notification := range toDismiss {
		if _, err := client.Activity.DeleteThreadSubscription(ctx, *notification.ID); err != nil {
			return fmt.Errorf("failed to unsubscribe from notification %q: %w", *notification.ID, err)
		}
		if _, err := client.Activity.MarkThreadRead(ctx, *notification.ID); err != nil {
			return fmt.Errorf("failed to mark notification %q as read: %w", *notification.ID, err)
		}
		l.Sugar().Info("unsubscribed from notification and marked it as read ", *notification.Subject.URL)
	}

	return nil
}
