//  Copyright (c) 2015 Couchbase, Inc.
//  Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file
//  except in compliance with the License. You may obtain a copy of the License at
//    http://www.apache.org/licenses/LICENSE-2.0
//  Unless required by applicable law or agreed to in writing, software distributed under the
//  License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
//  either express or implied. See the License for the specific language governing permissions
//  and limitations under the License.

package db

import (
	"fmt"
	"time"

	"github.com/couchbase/sync_gateway/base"
	"github.com/couchbase/sync_gateway/channels"
)

// Returns the (ordered) union of all of the changes made to multiple channels.
func (db *Database) VectorMultiChangesFeed(chans base.Set, options ChangesOptions) (<-chan *ChangeEntry, error) {
	to := ""
	var userVbNo uint16
	userLogging := "UserLogging"
	if db.user != nil && db.user.Name() != "" {
		to = fmt.Sprintf("  (to %s)", db.user.Name())
		userVbNo = uint16(db.Bucket.VBHash(db.user.DocID()))
		userLogging = db.user.Name()
	}

	base.LogTo("Changes+", "Vector MultiChangesFeed(%s, %+v) ... %s", chans, options, to)
	base.LogTo(userLogging, "Vector MultiChangesFeed(%s, %+v) ... %s", chans, options, to)
	output := make(chan *ChangeEntry, 50)

	go func() {
		defer func() {
			base.LogTo("Changes+", "MultiChangesFeed done %s", to)
			close(output)
		}()

		var changeWaiter *changeWaiter
		var userChangeCount uint64
		var addedChannels base.Set // Tracks channels added to the user during changes processing.

		if options.Wait {
			// Note (Adam): I don't think there's a reason to set this to false here.  We're outside the
			// main iteration loop (so the if check above should only happen once), and I don't believe
			// options.Wait is referenced elsewhere once MultiChangesFeed is called.  Leaving it as-is
			// makes it possible for channels to identify whether a getChanges call has options.Wait set to true,
			// which is useful to identify active change listeners.  However, it's possible there's a subtlety of
			// longpoll or continuous processing I'm missing here - leaving this note instead of just deleting for now.
			//options.Wait = false
			changeWaiter = db.startChangeWaiter(chans)
			userChangeCount = changeWaiter.CurrentUserCount()
		}

		cumulativeClock := getChangesClock(options.Since).Copy()

		// This loop is used to re-run the fetch after every database change, in Wait mode
	outer:
		for {
			if userLogging != "UserLogging" {
				base.LogTo(userLogging, "Outer iteration starts, since:%s", base.PrintClock(options.Since.Clock))
			}
			iterationStartTime := time.Now()
			// Restrict to available channels, expand wild-card, and find since when these channels
			// have been available to the user:
			var channelsSince channels.TimedSet
			if db.user != nil {
				channelsSince = db.user.FilterToAvailableChannels(chans)
			} else {
				channelsSince = channels.AtSequence(chans, 0)
			}

			// Updates the changeWaiter to the current set of available channels
			if changeWaiter != nil {
				changeWaiter.UpdateChannels(channelsSince)
			}
			base.LogTo("Changes+", "MultiChangesFeed: channels expand to %#v ... %s", channelsSince, to)

			// Populate the  array of feed channels:
			feeds := make([]<-chan *ChangeEntry, 0, len(channelsSince))

			base.LogTo("Changes+", "GotChannelSince... %v", channelsSince)
			for name, vbSeqAddedAt := range channelsSince {
				seqAddedAt := vbSeqAddedAt.Sequence
				// If there's no vbNo on the channelsSince, it indicates a user doc channel grant - use the userVbNo.
				var vbAddedAt uint16
				if vbSeqAddedAt.VbNo == nil {
					vbAddedAt = userVbNo
				} else {
					vbAddedAt = *vbSeqAddedAt.VbNo
				}

				base.LogTo("Changes+", "Starting for channel... %s, %d", name, seqAddedAt)
				chanOpts := options

				// Check whether requires backfill based on addedChannels in this _changes feed
				isNewChannel := false
				if addedChannels != nil {
					_, isNewChannel = addedChannels[name]
				}

				// Three possible scenarios for backfill handling, based on whether the incoming since value indicates a backfill in progress
				// for this channel, and whether the channel requires a new backfill to be started
				//   Case 1. No backfill in progress, no backfill required - use the incoming since to get changes
				//   Case 2. No backfill in progress, backfill required for this channel.  Get changes since zero, backfilling to the incoming since
				//   Case 3. Backfill in progress.  Get changes since zero, backfilling to incoming triggered by, filtered to later than incoming since.
				backfillInProgress := false
				if options.Since.TriggeredByClock != nil {
					// There's a backfill in progress for SOME channel - check if it's this one
					if options.Since.TriggeredByClock.GetSequence(vbAddedAt) == seqAddedAt {
						backfillInProgress = true
					}
				}

				sinceSeq := getChangesClock(options.Since).GetSequence(vbAddedAt)
				backfillRequired := vbSeqAddedAt.Sequence > 0 && sinceSeq < seqAddedAt

				if isNewChannel || (backfillRequired && !backfillInProgress) {
					// Case 2.  No backfill in progress, backfill required
					base.LogTo("Changes+", "Starting backfill for channel... %s, %d", name, seqAddedAt)

					base.LogTo(userLogging, "Starting backfill for channel %s for user %s", name, userLogging)
					chanOpts.Since = SequenceID{
						Seq:              0,
						vbNo:             0,
						Clock:            base.NewSequenceClockImpl(),
						TriggeredBy:      seqAddedAt,
						TriggeredByVbNo:  vbAddedAt,
						TriggeredByClock: getChangesClock(options.Since).Copy(),
					}
				} else if backfillInProgress {
					// Case 3.  Backfill in progress.
					base.LogTo(userLogging, "Backfill in progress for channel %s for user %s", name, userLogging)
					chanOpts.Since = SequenceID{
						Seq:              options.Since.Seq,
						vbNo:             options.Since.vbNo,
						Clock:            base.NewSequenceClockImpl(),
						TriggeredBy:      seqAddedAt,
						TriggeredByVbNo:  vbAddedAt,
						TriggeredByClock: options.Since.TriggeredByClock,
					}
				} else {
					// Case 1.  Leave chanOpts.Since set to options.Since.
					base.LogTo(userLogging, "No backfill for channel %s for user %s", name, userLogging)
				}
				feed, err := db.vectorChangesFeed(name, chanOpts, userLogging)
				if err != nil {
					base.Warn("MultiChangesFeed got error reading changes feed %q: %v", name, err)
					return
				}
				feeds = append(feeds, feed)
			}

			// If the user object has changed, create a special pseudo-feed for it:
			if db.user != nil {
				feeds, _ = db.appendVectorUserFeed(feeds, []string{}, options, userVbNo)
			}

			current := make([]*ChangeEntry, len(feeds))
			// This loop reads the available entries from all the feeds in parallel, merges them,
			// and writes them to the output channel:
			var sentSomething bool
			for {
				// Read more entries to fill up the current[] array:
				for i, cur := range current {
					if cur == nil && feeds[i] != nil {
						var ok bool
						current[i], ok = <-feeds[i]
						if !ok {
							feeds[i] = nil
						}
					}
				}

				// Find the current entry with the minimum sequence:
				minSeq := MaxSequenceID
				var minEntry *ChangeEntry
				for _, cur := range current {
					if cur != nil && cur.Seq.Before(minSeq) {
						minSeq = cur.Seq
						minEntry = cur
					} else {

						base.LogTo(userLogging, "Not sending because not minimum sequence:%v, %v", cur, minSeq)
					}
				}

				base.LogTo(userLogging, "minEntry:")
				if minEntry == nil {
					break // Exit the loop when there are no more entries
				}

				// Clear the current entries for the sequence just sent:
				for i, cur := range current {
					if cur != nil && cur.Seq == minSeq {
						current[i] = nil
						// Also concatenate the matching entries' Removed arrays:
						if cur != minEntry && cur.Removed != nil {
							if minEntry.Removed == nil {
								minEntry.Removed = cur.Removed
							} else {
								minEntry.Removed = minEntry.Removed.Union(cur.Removed)
							}
						}
					}
				}

				// Add the doc body or the conflicting rev IDs, if those options are set:
				if options.IncludeDocs || options.Conflicts {
					db.addDocToChangeEntry(minEntry, options)
				}

				if minEntry.Seq.TriggeredBy == 0 {
					// Update the cumulative clock, and stick it on the entry.
					cumulativeClock.SetMaxSequence(minEntry.Seq.vbNo, minEntry.Seq.Seq)
					clockHash, err := db.SequenceHasher.GetHash(cumulativeClock)
					// Change entries only need the hash value, not the full clock.  Creating a new
					// clock here to avoid the overhead of cumulativeClock.copy()
					minEntry.Seq.Clock = &base.SequenceClockImpl{}
					if err != nil {
						base.Warn("Error calculating hash for clock:%v", base.PrintClock(minEntry.Seq.Clock))
					} else {
						minEntry.Seq.Clock.SetHashedValue(clockHash)
					}
				} else {
					// For backfill (triggered by), we don't want to update the cumulative clock.  All entries triggered by the
					// same sequence reference the same triggered by clock, so it should only need to get hashed once.
					// If this is the first entry for this triggered by, initialize the triggered by clock's
					// hash value.
					if minEntry.Seq.TriggeredByClock.GetHashedValue() == "" {
						cumulativeClock.SetMaxSequence(minEntry.Seq.TriggeredByVbNo, minEntry.Seq.TriggeredBy)
						clockHash, err := db.SequenceHasher.GetHash(cumulativeClock)
						if err != nil {
							base.Warn("Error calculating hash for triggered by clock:%v", base.PrintClock(cumulativeClock))
						} else {
							minEntry.Seq.TriggeredByClock.SetHashedValue(clockHash)
						}
					}
				}
				// Send the entry, and repeat the loop:
				select {
				case <-options.Terminator:
					return
				case output <- minEntry:
					base.LogTo(userLogging, "vectorChangesFeed, wrote entry [%v][%v]", minEntry.ID, minEntry.Seq)
				}
				sentSomething = true

				// Stop when we hit the limit (if any):
				if options.Limit > 0 {
					options.Limit--
					if options.Limit == 0 {
						break outer
					}
				}
				// Update options.Since for use in the next outer loop iteration.
				options.Since.Clock = cumulativeClock
			}

			if !options.Continuous && (sentSomething || changeWaiter == nil) {
				break
			}

			// If nothing found, and in wait mode: wait for the db to change, then run again.
			// First notify the reader that we're waiting by sending a nil.
			base.LogTo("Changes+", "MultiChangesFeed waiting... %s", to)
			output <- nil
			if !changeWaiter.Wait() {
				break
			}

			// Check whether I was terminated while waiting for a change:
			select {
			case <-options.Terminator:
				return
			default:
			}

			// Before checking again, update the User object in case its channel access has
			// changed while waiting:
			var err error
			userChangeCount, addedChannels, err = db.checkForUserUpdates(userChangeCount, changeWaiter)
			if err != nil {
				change := makeErrorEntry("User not found during reload - terminating changes feed")
				base.LogTo("Changes+", "User not found during reload - terminating changes feed with entry %+v", change)
				output <- &change
				return
			}
			writeHistogram(indexTimingExpvars, iterationStartTime, "index_changes_iterationTime")
		}
	}()

	return output, nil
}

// Creates a Go-channel of all the changes made on a channel.
// Does NOT handle the Wait option. Does NOT check authorization.
func (db *Database) vectorChangesFeed(channel string, options ChangesOptions, userLogging string) (<-chan *ChangeEntry, error) {
	dbExpvars.Add("channelChangesFeeds", 1)
	log, err := db.changeCache.GetChanges(channel, options)
	base.LogTo("Changes+", "[changesFeed] Found %d changes for channel %s", len(log), channel)
	base.LogTo(userLogging, "[changesFeed] Found %d changes for channel %s (%s)", len(log), channel, userLogging)

	if err != nil {
		return nil, err
	}

	if len(log) == 0 {
		// There are no entries newer than 'since'. Return an empty feed:
		feed := make(chan *ChangeEntry)
		close(feed)
		return feed, nil
	}

	feed := make(chan *ChangeEntry, 1)
	go func() {
		defer close(feed)

		// Send backfill first
		if options.Since.TriggeredByClock != nil {
			for i := 0; i < len(log); i++ {
				logEntry := log[i]
				// If sequence is less than the backfillTo clock sequence for its vbucket, send as backfill (i.e. with triggered by)
				isBackfill := logEntry.Sequence <= options.Since.TriggeredByClock.GetSequence(logEntry.VbNo)

				// Only send backfill that's hasn't already been sent (i.e. after the sequence part of options.Since)
				isPending := options.Since.VbucketSequenceBefore(logEntry.VbNo, logEntry.Sequence)

				if isBackfill && isPending {
					seqID := SequenceID{
						SeqType:          ClockSequenceType,
						Seq:              logEntry.Sequence,
						vbNo:             logEntry.VbNo,
						TriggeredBy:      options.Since.TriggeredBy,
						TriggeredByVbNo:  options.Since.TriggeredByVbNo,
						TriggeredByClock: options.Since.TriggeredByClock,
					}
					change := makeChangeEntry(logEntry, seqID, channel)
					select {
					case <-options.Terminator:
						base.LogTo("Changes+", "Aborting changesFeed")
						return
					case feed <- &change:
					}
				}
				if isBackfill {
					// remove from the set, so that it's not resent below
					log[i] = nil
				}
			}
		}

		// Now send any remaining entries
		for _, logEntry := range log {
			// Ignore any already sent as backfill
			if logEntry != nil {
				seqID := SequenceID{
					SeqType: ClockSequenceType,
					Seq:     logEntry.Sequence,
					vbNo:    logEntry.VbNo,
				}
				change := makeChangeEntry(logEntry, seqID, channel)
				select {
				case <-options.Terminator:
					base.LogTo("Changes+", "Aborting changesFeed")
					return
				case feed <- &change:
				}
			}
		}
	}()
	return feed, nil
}

func (db *Database) appendVectorUserFeed(feeds []<-chan *ChangeEntry, names []string, options ChangesOptions, userVbNo uint16) ([]<-chan *ChangeEntry, []string) {

	if db.user.Sequence() > 0 {
		userSeq := SequenceID{
			SeqType: ClockSequenceType,
			Seq:     db.user.Sequence(),
			vbNo:    userVbNo,
		}

		// Get since sequence for the userSeq's vbucket
		sinceSeq := getChangesClock(options.Since).GetSequence(userVbNo)

		if sinceSeq < userSeq.Seq {

			name := db.user.Name()
			if name == "" {
				name = base.GuestUsername
			}
			entry := ChangeEntry{
				Seq:     userSeq,
				ID:      "_user/" + name,
				Changes: []ChangeRev{},
			}
			base.LogTo(name, "Sending user doc to user feed")
			userFeed := make(chan *ChangeEntry, 1)
			userFeed <- &entry
			close(userFeed)
			feeds = append(feeds, userFeed)
			names = append(names, entry.ID)
		}
	}
	return feeds, names
}

func getChangesClock(sequence SequenceID) base.SequenceClock {
	if sequence.TriggeredByClock != nil {
		return sequence.TriggeredByClock
	} else {
		return sequence.Clock
	}
}