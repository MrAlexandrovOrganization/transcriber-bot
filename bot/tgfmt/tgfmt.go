// Package tgfmt separates trusted Telegram HTML from dynamic plain text.
package tgfmt

import "html"

type HTML string

// Escape must be applied to dynamic text before sending it in HTML parse mode.
func Escape(text string) HTML { return HTML(html.EscapeString(text)) }
