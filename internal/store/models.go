package store

import (
	"crypto/rand"
	"time"

	"github.com/oklog/ulid/v2"
)

const (
	PrefixUser                = "usr_"
	PrefixSession             = "ses_"
	PrefixAPIToken            = "tok_"
	PrefixSetting             = "set_"
	PrefixEngine              = "eng_"
	PrefixCategory            = "cat_"
	PrefixTag                 = "tag_"
	PrefixTaskTracker         = "ttr_"
	PrefixTaskEvent           = "evt_"
	PrefixIndexer             = "idx_"
	PrefixSearchJob           = "sch_"
	PrefixSearchResult        = "res_"
	PrefixFeed                = "fed_"
	PrefixFeedItem            = "itm_"
	PrefixTask                = "tsk_"
	PrefixNotificationChannel = "ntf_"
	PrefixRule                = "rul_"
	PrefixRuleMatch           = "mat_"
	PrefixJob                 = "job_"
	PrefixBandwidthCell       = "bws_"
	PrefixUIPref              = "uip_"
	PrefixWatchFolder         = "wfd_"
	PrefixTaskFile            = "tfi_"
)

// NewID returns a table prefix followed by a Crockford base32 ULID.
func NewID(prefix string) string {
	id := ulid.MustNew(ulid.Timestamp(time.Now()), rand.Reader)

	return prefix + id.String()
}
