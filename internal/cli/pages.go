package cli

import (
	"net/url"
	"strings"

	"github.com/spf13/cobra"
)

// allPages reads every page of a cursor-paged list (design 04 §1): GET path,
// then again with each next_cursor until it comes back empty. page decodes one
// response into the caller's accumulator and returns its next_cursor.
//
// A CLI listing is asked for everything, so it reads everything, the largest
// page at a time.
func allPages(c *Client, path string, page func(get func(out any) error) (string, error)) error {
	cursor := ""
	for {
		p := path
		sep := "?"
		if strings.Contains(p, "?") {
			sep = "&"
		}
		p += sep + "limit=500"
		if cursor != "" {
			p += "&cursor=" + url.QueryEscape(cursor)
		}
		next, err := page(func(out any) error { return c.Do("GET", p, nil, out) })
		if err != nil {
			return err
		}
		if next == "" {
			return nil
		}
		cursor = next
	}
}

// printAllPages reads every page of a list and prints its items together,
// under the key each page carries them in.
func printAllPages(cmd *cobra.Command, c *Client, path, key string) error {
	items := []any{}
	err := allPages(c, path, func(get func(any) error) (string, error) {
		var page map[string]any
		if err := get(&page); err != nil {
			return "", err
		}
		more, _ := page[key].([]any)
		items = append(items, more...)
		next, _ := page["next_cursor"].(string)
		return next, nil
	})
	if err != nil {
		return err
	}
	return printJSON(cmd.OutOrStdout(), map[string]any{key: items})
}
