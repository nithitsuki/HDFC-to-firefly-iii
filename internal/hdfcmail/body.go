package hdfcmail

import (
	"io"
	"strings"

	"github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset" // register charset decoders
	"golang.org/x/net/html"
)

// Body walks a MIME entity and returns the most useful human-readable text it
// contains. A text/plain part is preferred, and a text/html part is flattened
// when there is no usable plain part.
//
// HDFC's alerts make the HTML path the common case, not the fallback: the
// messages are multipart/alternative carrying a single text/html part and no
// text/plain part at all.
//
// The tree is walked exactly once. message.Entity.MultipartReader returns a
// reader over the entity's body, and reading it consumes that body, so a
// second walk would see an empty message.
func Body(e *message.Entity) string {
	var plain, htm string
	collect(e, &plain, &htm)

	if plain != "" {
		return plain
	}
	if htm != "" {
		return HTMLToText(htm)
	}
	return ""
}

// collect walks the MIME tree once, keeping the first text/plain and the first
// text/html part it finds.
func collect(e *message.Entity, plain, htm *string) {
	if e == nil {
		return
	}
	mediaType, _, err := e.Header.ContentType()
	if err != nil {
		mediaType = "text/plain"
	}
	mediaType = strings.ToLower(mediaType)

	switch {
	case mediaType == "text/plain":
		if *plain == "" {
			*plain = readAll(e.Body)
		}
	case mediaType == "text/html":
		if *htm == "" {
			*htm = readAll(e.Body)
		}
	case strings.HasPrefix(mediaType, "multipart/"):
		mr := e.MultipartReader()
		if mr == nil {
			return
		}
		for {
			part, err := mr.NextPart()
			if err != nil {
				// io.EOF means the multipart is finished; anything else means
				// it is malformed, and a partial body still beats none.
				return
			}
			collect(part, plain, htm)
		}
	}
}

func readAll(r io.Reader) string {
	if r == nil {
		return ""
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return ""
	}
	return string(b)
}

// HTMLToText flattens an HTML document into plain text, inserting line breaks
// at block boundaries. It is deliberately forgiving: bank emails are machine
// generated but not always valid HTML.
func HTMLToText(src string) string {
	doc, err := html.Parse(strings.NewReader(src))
	if err != nil {
		return ""
	}

	var b strings.Builder
	var visit func(*html.Node)
	visit = func(n *html.Node) {
		switch n.Type {
		case html.TextNode:
			b.WriteString(n.Data)
		case html.ElementNode:
			switch n.Data {
			case "br":
				b.WriteByte('\n')
			case "p", "div", "tr", "table", "li", "h1", "h2", "h3", "h4":
				b.WriteByte('\n')
			case "script", "style", "head":
				return // skip subtree entirely
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			visit(c)
		}
		if n.Type == html.ElementNode {
			switch n.Data {
			case "p", "div", "tr", "table", "li":
				b.WriteByte('\n')
			}
		}
	}
	visit(doc)

	return collapseBlankLines(b.String())
}

// collapseBlankLines trims each line and drops runs of empty lines, so that
// regexes can be written against predictable whitespace.
func collapseBlankLines(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	blank := true // suppress leading blanks
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			if blank {
				continue
			}
			blank = true
			out = append(out, "")
			continue
		}
		blank = false
		out = append(out, line)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}
