// Package tgfmt separates trusted Telegram HTML from dynamic plain text.
package tgfmt

import "html"

type HTML string

// Escape must be applied to dynamic text before sending it in HTML parse mode.
func Escape(text string) HTML { return HTML(html.EscapeString(text)) }

// Blockquote wraps already-escaped HTML in a Telegram block quote.
// The quoted text must occupy the whole message or start on its own line.
func Blockquote(h HTML) HTML { return "<blockquote>" + h + "</blockquote>" }
