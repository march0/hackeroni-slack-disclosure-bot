package main

import (
	"fmt"
	"log"
	"os"
	"time"

	hackeroni "github.com/bored-engineer/hackeroni/legacy"
	slack "github.com/bored-engineer/slack-incoming-webhooks"
	"github.com/robmccoll/mitlru"
)

func main() {

	// Setup the Slack client
	api := slack.Client{
		WebhookURL: os.Getenv("SLACK_WEBHOOK_URL"),
	}

	// Create a new HackerOne client (legacy)
	client := hackeroni.NewClient(nil)

	// We need the HackerOne icon so lookup that team (named "security")
	team, _, err := client.Team.Get("security")
	if err != nil {
		log.Fatalf("client.Team.Get failed: %v", err)
	}

	// The interval to refresh reports
	interval := 2 * time.Minute
	window := 2 * time.Minute

	// Hold a LRU cache of report IDs (100 should be fine)
	cache := mitlru.NewTTLRUCache(100, interval+window)

	// Poll for new hacktivity every interval
	for range time.Tick(interval) {

		// Look for all activity since now minus the window
		updatedSince := time.Now().UTC().Add(-window)

		// List all disclosed Hacktivity since last check starting at page 1
		opts := hackeroni.HacktivityListOptions{
			Filter:   hackeroni.HacktivityFilterDisclosed,
			SortType: hackeroni.HacktivitySortTypeLatestDisclosableActivityAt,
			Page:     1,
		}
		var allReports []*hackeroni.Report
	PageLoop:
		for {
			// Actually list
			reports, _, err := client.Hacktivity.List(opts)
			if err != nil {
				log.Printf("client.Hacktivity.List failed: %v", err)
				break PageLoop
			}

			// Add every new report
			for idx, report := range reports {
				reportTime := report.LatestDisclosableActivityAt.Time
				if reportTime.Before(updatedSince) {
					allReports = append(allReports, reports[0:idx]...)
					break PageLoop
				}
			}
			allReports = append(allReports, reports...)

			// Paginate
			opts.Page += 1
		}

		// Loop each report
	ReportLoop:
		for _, report := range allReports {
			// Check if we've seen the report already, if so skip it
			_, seen := cache.Get(*report.ID)
			if seen {
				continue
			}
			// Retrieve the full report
			fullReport, _, err := client.Report.Get(*report.ID)
			if err != nil {
				log.Printf("client.Report.Get failed: %v", err)
				continue ReportLoop
			}

			// Lookup the reporter
			reporter, _, err := client.User.Get(*report.Reporter.Username)
			if err != nil {
				log.Printf("client.User.Get failed: %v", err)
				continue ReportLoop
			}

			// Build the message attachment
			attachment := slack.Attachment{
				Fallback:   *report.URL,
				AuthorName: fmt.Sprintf("%s (%s)", *report.Reporter.Username, *reporter.Name),
				AuthorLink: *report.Reporter.URL,
				AuthorIcon: *reporter.ProfilePictureURLs.Best(),
				Title:      fmt.Sprintf("Report %d: %s", *report.ID, *report.Title),
				TitleLink:  *report.URL,
				Footer:     "HackerOne Disclosure Bot",
				FooterIcon: *team.ProfilePictureURLs.Best(),
			}

			// Loop each summary and add the researchers's summary if it exists
			for _, summary := range fullReport.Summaries {
				if summary.Category == nil {
					continue
				}
				if summary.Content == nil {
					continue
				}
				switch *summary.Category {
				case hackeroni.ReportSummaryCategoryTeam:
					attachment.Pretext = *summary.Content
				case hackeroni.ReportSummaryCategoryResearcher:
					attachment.Text = *summary.Content
				}
			}

			// Set the attachment color based on the report state (extracted from CSS)
			switch *report.Substate {
			case hackeroni.ReportStateNew:
				attachment.Color = "#8e44ad"
			case hackeroni.ReportStateTriaged:
				attachment.Color = "#e67e22"
			case hackeroni.ReportStateResolved:
				attachment.Color = "#609828"
			case hackeroni.ReportStateNotApplicable:
				attachment.Color = "#ce3f4b"
			case hackeroni.ReportStateInformative:
				attachment.Color = "#ccc"
			case hackeroni.ReportStateDuplicate:
				attachment.Color = "#a78260"
			case hackeroni.ReportStateSpam:
				attachment.Color = "#555"
			}

			// If the report has a bounty, add it
			if fullReport.HasBounty != nil && *fullReport.HasBounty {
				attachment.AddField(&slack.Field{
					Title: "Bounty",
					Value: *fullReport.FormattedBounty,
					Short: true,
				})
			}

			// If we have the exact time it was disclosed add that
			if fullReport.DisclosedAt != nil {
				attachment.Timestamp = fullReport.DisclosedAt.Unix()
			}

			// Post the actual message
			err = api.Post(&slack.Payload{
				Username:    fmt.Sprintf("%s Disclosed", *report.Team.Profile.Name),
				IconURL:     *report.Team.ProfilePictureURLs.Best(),
				Attachments: []*slack.Attachment{&attachment},
			})
			if err != nil {
				log.Printf("api.Post failed: %v", err)
				continue ReportLoop
			}

			// Add to cache
			cache.Add(*report.ID, true)
		}
	}

}
