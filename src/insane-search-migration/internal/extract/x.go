package extract

import (
	"encoding/json"
	"regexp"
	"strings"
)

var nextDataRe = regexp.MustCompile(`(?s)<script id="__NEXT_DATA__" type="application/json">(.*?)</script>`)

type xTweet struct {
	IDStr     string          `json:"id_str"`
	FullText  string          `json:"full_text"`
	Permalink string          `json:"permalink"`
	InReplyTo string          `json:"in_reply_to_status_id_str"`
	Retweeted json.RawMessage `json:"retweeted_status"`
	Entities  struct {
		URLs []struct {
			URL         string `json:"url"`
			ExpandedURL string `json:"expanded_url"`
		} `json:"urls"`
	} `json:"entities"`
}

type xTimelinePage struct {
	Props struct {
		PageProps struct {
			Timeline struct {
				Entries []struct {
					Content struct {
						Tweet *xTweet `json:"tweet"`
					} `json:"content"`
				} `json:"entries"`
			} `json:"timeline"`
		} `json:"pageProps"`
	} `json:"props"`
}

// xListing parses the public syndication profile timeline
// (https://syndication.twitter.com/srv/timeline-profile/screen-name/<handle>),
// whose __NEXT_DATA__ payload carries full tweet text without an account.
// Only top-level posts become rows: replies are conversational noise and
// retweets carry truncated text, so both are skipped. A tweet's text is the
// whole post, so it serves as both the title and the listing excerpt.
func xListing(source, text string) ([]Row, ListingDiagnostics) {
	rows := []Row{}
	diag := ListingDiagnostics{Format: "x-timeline"}
	m := nextDataRe.FindStringSubmatch(text)
	if m == nil {
		diag.Error = "unrecognized_listing_x"
		return rows, diag
	}
	var page xTimelinePage
	if err := json.Unmarshal([]byte(m[1]), &page); err != nil {
		diag.Error = "invalid_x_timeline_json"
		return rows, diag
	}
	entries := page.Props.PageProps.Timeline.Entries
	if entries == nil {
		diag.Error = "unrecognized_listing_x"
		return rows, diag
	}
	diag.Recognized = true
	for _, e := range entries {
		tw := e.Content.Tweet
		if tw == nil || tw.IDStr == "" || tw.Permalink == "" {
			continue
		}
		if len(tw.Retweeted) > 0 && string(tw.Retweeted) != "null" {
			continue
		}
		if tw.InReplyTo != "" {
			continue
		}
		body := tw.FullText
		for _, u := range tw.Entities.URLs {
			if u.URL != "" && u.ExpandedURL != "" {
				body = strings.ReplaceAll(body, u.URL, u.ExpandedURL)
			}
		}
		body = StripPoison(CleanText(body))
		if body == "" {
			continue
		}
		rows = append(rows, Row{source, body, "https://x.com" + tw.Permalink, body})
	}
	diag.Parsed = len(rows)
	return rows, diag
}
