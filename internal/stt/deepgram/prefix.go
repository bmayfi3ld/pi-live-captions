package deepgram

import (
	"strings"
	"time"

	"livecaption/internal/stt"
)

// holdbackTokens is how many trailing words a stable prefix withholds before
// publishing. Two interims agreeing on a word is not quite enough evidence by
// itself: Deepgram commonly rewrites the last word or two of an interim as
// more audio and context arrive, so a short trailing window is kept back
// every round rather than trusted the moment it first agrees.
const holdbackTokens = 2

// prefixTracker turns one connection's revisable interim stream into the
// append-only sequence stt.Transcript promises. Deepgram interims are free to
// rewrite earlier words as a window continues; nothing downstream may see a
// word that later disappears. It buys that guarantee by only publishing the
// prefix of tokens that has proven stable across two consecutive interims
// (minus holdbackTokens), and by publishing everything left over — including
// the still-uncertain tail — the instant the window's is_final message
// arrives, since Deepgram will not revise it again after that.
//
// Lifetime matches anchorIndex: built in runConnection and discarded with the
// connection, so a reconnect starts a clean window rather than diffing
// against a stale one.
type prefixTracker struct {
	prevTokens []string // previous interim's tokens, for prefix comparison
	emitted    int      // token count already published, by position in the window

	open    bool          // a window is in progress; cleared on is_final
	lastEnd time.Duration // media time already accounted for by a publish; Start of the next one
}

func newPrefixTracker() *prefixTracker { return &prefixTracker{} }

// update consumes one decoded message from the current window — interim or
// final — and reports the Transcript newly safe to publish, if any. ok is
// false when the message added nothing past what was already published: the
// common case for an interim that only extends the still-held-back tail
// rather than confirming new prefix.
//
// Start/Duration on the returned Transcript cover only the newly published
// tokens, not the whole message: they pick up where the previous publish for
// this window left off and run to t.End(), so successive publishes tile the
// window without gaps or overlap and hub's gap/break arithmetic keeps
// working unmodified.
func (p *prefixTracker) update(t stt.Transcript, isFinal bool) (stt.Transcript, bool) {
	tokens := strings.Fields(t.Text)

	if !p.open {
		p.lastEnd = t.Start
		p.open = true
	}

	// On is_final, publish everything regardless of agreement — there is no
	// next interim left to agree with, and Deepgram will not revise this
	// window again. Otherwise, only the prefix two consecutive interims agree
	// on is trustworthy, and even that keeps a trailing holdback back.
	cut := len(tokens)
	if !isFinal {
		cut = commonPrefixLen(p.prevTokens, tokens) - holdbackTokens
	}
	cut = max(cut, p.emitted)
	cut = min(cut, len(tokens))

	var out stt.Transcript
	published := false
	if cut > p.emitted {
		// Deepgram rounds start+duration to 2-3 decimals (see anchor.go), so a
		// final can land a few ms short of the last interim's End. That is
		// rounding, not a real rewind, hence the floor at zero.
		dur := max(t.End()-p.lastEnd, 0)
		out = stt.Transcript{
			Text:       strings.Join(tokens[p.emitted:cut], " "),
			Start:      p.lastEnd,
			Duration:   dur,
			Confidence: t.Confidence,
		}
		p.lastEnd = t.End()
		p.emitted = cut
		published = true
	}

	if isFinal {
		p.prevTokens = nil
		p.emitted = 0
		p.open = false
	} else {
		p.prevTokens = tokens
	}

	return out, published
}

// commonPrefixLen reports how many leading tokens a and b agree on.
func commonPrefixLen(a, b []string) int {
	n := min(len(a), len(b))
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	return i
}
