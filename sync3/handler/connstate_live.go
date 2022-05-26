package handler

import (
	"context"
	"encoding/json"
	"runtime/trace"
	"strings"
	"time"

	"github.com/matrix-org/sync-v3/internal"
	"github.com/matrix-org/sync-v3/sync3"
	"github.com/matrix-org/sync-v3/sync3/caches"
	"github.com/matrix-org/sync-v3/sync3/extensions"
)

var (
	// The max number of events the client is eligible to read (unfiltered) which we are willing to
	// buffer on this connection. Too large and we consume lots of memory. Too small and busy accounts
	// will trip the connection knifing.
	MaxPendingEventUpdates = 200
)

// Contains code for processing live updates. Split out from connstate because they concern different
// code paths. Relies on ConnState for various list/sort/subscription operations.
type connStateLive struct {
	*ConnState

	// A channel which the dispatcher uses to send updates to the conn goroutine
	// Consumed when the conn is read. There is a limit to how many updates we will store before
	// saying the client is dead and clean up the conn.
	updates    chan caches.Update
	bufferFull bool
}

// Called when there is an update from the user cache. This callback fires when the server gets a new event and determines this connection MAY be
// interested in it (e.g the client is joined to the room or it's an invite, etc).
// We need to move this data onto a channel for onIncomingRequest to consume later.
func (s *connStateLive) onUpdate(up caches.Update) {
	if s.bufferFull {
		return
	}
	select {
	case s.updates <- up:
	case <-time.After(5 * time.Second):
		logger.Warn().Interface("update", up).Str("user", s.userID).Msg(
			"cannot send update to connection, buffer exceeded. Destroying connection.",
		)
		s.bufferFull = true
		s.Destroy()
	}
}

// live update waits for new data and populates the response given when new data arrives.
func (s *connStateLive) liveUpdate(
	ctx context.Context, req *sync3.Request, ex extensions.Request, isInitial bool,
	response *sync3.Response,
) {
	// we need to ensure that we keep consuming from the updates channel, even if they want a response
	// immediately. If we have new list data we won't wait, but if we don't then we need to be able to
	// catch-up to the current head position, hence giving 100ms grace period for processing.
	if req.TimeoutMSecs() < 100 {
		req.SetTimeoutMSecs(100)
	}
	// block until we get a new event, with appropriate timeout
	startTime := time.Now()
	for response.ListOps() == 0 && len(response.Rooms) == 0 && !response.Extensions.HasData(isInitial) {
		timeToWait := time.Duration(req.TimeoutMSecs()) * time.Millisecond
		timeWaited := time.Since(startTime)
		timeLeftToWait := timeToWait - timeWaited
		if timeLeftToWait < 0 {
			logger.Trace().Str("user", s.userID).Str("time_waited", timeWaited.String()).Msg("liveUpdate: timed out")
			return
		}
		logger.Trace().Str("user", s.userID).Str("dur", timeLeftToWait.String()).Msg("liveUpdate: no response data yet; blocking")
		select {
		case <-ctx.Done(): // client has given up
			logger.Trace().Str("user", s.userID).Msg("liveUpdate: client gave up")
			trace.Logf(ctx, "liveUpdate", "context cancelled")
			return
		case <-time.After(timeLeftToWait): // we've timed out
			logger.Trace().Str("user", s.userID).Msg("liveUpdate: timed out")
			trace.Logf(ctx, "liveUpdate", "timed out after %v", timeLeftToWait)
			return
		case update := <-s.updates:
			trace.Logf(ctx, "liveUpdate", "process live update")
			updateWillReturnResponse := s.processLiveUpdate(ctx, update, response)
			// pass event to extensions AFTER processing
			s.extensionsHandler.HandleLiveUpdate(update, ex, &response.Extensions, updateWillReturnResponse, isInitial)
			// if there's more updates and we don't have lots stacked up already, go ahead and process another
			for len(s.updates) > 0 && response.ListOps() < 50 {
				update = <-s.updates
				willReturn := s.processLiveUpdate(ctx, update, response)
				if willReturn {
					updateWillReturnResponse = true
				}
				s.extensionsHandler.HandleLiveUpdate(update, ex, &response.Extensions, updateWillReturnResponse, isInitial)
			}
		}
	}
	logger.Trace().Str("user", s.userID).Int("subs", len(response.Rooms)).Msg("liveUpdate: returning")
	// TODO: op consolidation
}

func (s *connStateLive) processLiveUpdate(ctx context.Context, up caches.Update, response *sync3.Response) bool {
	hasUpdates := false
	internal.Assert("processLiveUpdate: response list length != internal list length", s.lists.Len() == len(response.Lists))
	internal.Assert("processLiveUpdate: request list length != internal list length", s.lists.Len() == len(s.muxedReq.Lists))

	// do global connection updates (e.g adding/removing rooms from allRooms)
	s.processGlobalUpdates(ctx, up)

	// process room subscriptions
	isSubscribedToRoom := false
	roomUpdate, ok := up.(*caches.RoomEventUpdate)
	if ok {
		if _, ok = s.roomSubscriptions[roomUpdate.RoomID()]; ok {
			// there is a subscription for this room, so update the room subscription response
			roomSub := *s.getDeltaRoomData(roomUpdate.RoomID(), roomUpdate.EventData.Event)
			response.Rooms[roomUpdate.RoomID()] = roomSub // TODO: consolidate updates
			isSubscribedToRoom = true
			hasUpdates = true
		}
	}

	// do per-list updates (e.g resorting, adding/removing rooms which no longer match filter)
	s.lists.ForEach(func(index int, list *sync3.FilteredSortableRooms) {
		reqList := s.muxedReq.Lists[index]
		if s.processLiveUpdateForList(ctx, up, &reqList, s.lists.List(index), &response.Lists[index], isSubscribedToRoom) {
			hasUpdates = true
		}
	})
	return hasUpdates
}

// this function does any updates which apply to the connection, regardless of which lists/subs exist.
func (s *connStateLive) processGlobalUpdates(ctx context.Context, up caches.Update) {
	// TODO: joins and leave?
	switch update := up.(type) {
	case *caches.RoomEventUpdate:
		// keep track of the latest stream position
		if update.EventData.LatestPos > s.loadPosition {
			s.loadPosition = update.EventData.LatestPos
		}
	case *caches.InviteUpdate:
		if update.Retired {
			// remove the room from all rooms
			logger.Trace().Str("user", s.userID).Str("room", update.RoomID()).Msg("processGlobalUpdates: room was retired")
			for i, r := range s.allRooms {
				if r.RoomID == update.RoomID() {
					// delete the room
					s.allRooms[i] = s.allRooms[len(s.allRooms)-1]
					s.allRooms = s.allRooms[:len(s.allRooms)-1]
				}
			}
		} else {
			// add the room to allRooms if it doesn't exist
			exists := false
			for _, r := range s.allRooms {
				if r.RoomID == update.RoomID() {
					exists = true
					break
				}
			}
			if !exists {
				logger.Trace().Str("user", s.userID).Str("room", update.RoomID()).Msg("processGlobalUpdates: newly invited room")
				// TODO: factor out
				metadata := update.InviteData.RoomMetadata()
				s.allRooms = append(s.allRooms, sync3.RoomConnMetadata{
					RoomMetadata: *metadata,
					UserRoomData: *update.UserRoomMetadata(),
					CanonicalisedName: strings.ToLower(
						strings.Trim(internal.CalculateRoomName(metadata, 5), "#!():_@"),
					),
				})
			}
		}
	}
}

func (s *connStateLive) processLiveUpdateForList(
	ctx context.Context, up caches.Update, reqList *sync3.RequestList, intList *sync3.FilteredSortableRooms, resList *sync3.ResponseList,
	isSubscribedToRoom bool,
) bool {
	hasUpdates := false
	roomUpdate, ok := up.(caches.RoomUpdate)
	if ok { // update the internal lists - this may remove rooms if the room no longer matches a filter
		// see if the latest room metadata means we delete a room, else update our state
		deletedIndex := intList.UpdateGlobalRoomMetadata(roomUpdate.GlobalRoomMetadata())
		if op := s.writeDeleteOp(reqList, deletedIndex); op != nil {
			resList.Ops = append(resList.Ops, op)
			hasUpdates = true
		}
		// see if the latest user room metadata means we delete a room (e.g it transition from dm to non-dm)
		// modify notification counts, DM-ness, etc
		deletedIndex = intList.UpdateUserRoomMetadata(roomUpdate.RoomID(), roomUpdate.UserRoomMetadata())
		if op := s.writeDeleteOp(reqList, deletedIndex); op != nil {
			resList.Ops = append(resList.Ops, op)
			hasUpdates = true
		}
	}

	switch update := up.(type) {
	case *caches.RoomEventUpdate:
		logger.Trace().Str("user", s.userID).Str("type", update.EventData.EventType).Msg("received event update")
		ops := s.processIncomingEventForList(ctx, update, reqList, intList, isSubscribedToRoom)
		resList.Ops = append(resList.Ops, ops...)
	case *caches.UnreadCountUpdate:
		logger.Trace().Str("user", s.userID).Str("room", update.RoomID()).Msg("received unread count update")
		ops := s.processUnreadCountUpdateForList(ctx, update, reqList, intList, isSubscribedToRoom)
		resList.Ops = append(resList.Ops, ops...)
	case *caches.InviteUpdate:
		logger.Trace().Str("user", s.userID).Str("room", update.RoomID()).Msg("received invite update")
		if update.Retired {
			// remove the room from this list if need be
			deletedIndex := intList.Remove(update.RoomID())
			if op := s.writeDeleteOp(reqList, deletedIndex); op != nil {
				resList.Ops = append(resList.Ops, op)
				hasUpdates = true
			}
		} else {
			roomUpdate := &caches.RoomEventUpdate{
				RoomUpdate: update.RoomUpdate,
				EventData:  update.InviteData.InviteEvent,
			}
			ops := s.processIncomingEventForList(ctx, roomUpdate, reqList, intList, isSubscribedToRoom)
			resList.Ops = append(resList.Ops, ops...)
		}
	}

	if !hasUpdates {
		hasUpdates = len(resList.Ops) > 0
	}

	return hasUpdates
}

func (s *connStateLive) processUnreadCountUpdateForList(
	ctx context.Context, up *caches.UnreadCountUpdate, reqList *sync3.RequestList, intList *sync3.FilteredSortableRooms,
	isSubscribedToRoom bool,
) []sync3.ResponseOp {
	if !up.HasCountDecreased {
		// if the count increases then we'll notify the user for the event which increases the count, hence
		// do nothing. We only care to notify the user when the counts decrease.
		return nil
	}

	fromIndex, ok := intList.IndexOf(up.RoomID())
	if !ok {
		return nil
	}
	return s.resort(ctx, reqList, intList, up.RoomID(), fromIndex, nil, false, false, isSubscribedToRoom)
}

func (s *connStateLive) processIncomingEventForList(
	ctx context.Context, update *caches.RoomEventUpdate, reqList *sync3.RequestList, intList *sync3.FilteredSortableRooms,
	isSubscribedToRoom bool,
) []sync3.ResponseOp {
	fromIndex, ok := intList.IndexOf(update.RoomID())
	newlyAdded := false
	if !ok {
		// the user may have just joined the room hence not have an entry in this list yet.
		fromIndex = int(intList.Len())
		roomMetadata := update.GlobalRoomMetadata()
		roomMetadata.RemoveHero(s.userID)
		newRoomConn := sync3.RoomConnMetadata{
			RoomMetadata: *roomMetadata,
			UserRoomData: *update.UserRoomMetadata(),
			CanonicalisedName: strings.ToLower(
				strings.Trim(internal.CalculateRoomName(roomMetadata, 5), "#!():_@"),
			),
		}
		if !intList.Add(newRoomConn) {
			// we didn't add this room to the list so we don't need to resort
			return nil
		}
		logger.Info().Str("room", update.RoomID()).Msg("room added")
		newlyAdded = true
	}
	return s.resort(ctx, reqList, intList, update.RoomID(), fromIndex, update.EventData.Event, newlyAdded, update.EventData.ForceInitial, isSubscribedToRoom)
}

// Resort should be called after a specific room has been modified in `sortedJoinedRooms`.
func (s *connStateLive) resort(
	ctx context.Context,
	reqList *sync3.RequestList, intList *sync3.FilteredSortableRooms, roomID string,
	fromIndex int, newEvent json.RawMessage, newlyAdded, forceInitial, isSubscribedToRoom bool,
) []sync3.ResponseOp {
	if reqList.Sort == nil {
		reqList.Sort = []string{sync3.SortByRecency}
	}
	if err := intList.Sort(reqList.Sort); err != nil {
		logger.Err(err).Msg("cannot sort list")
	}

	toIndex, _ := intList.IndexOf(roomID)
	isInsideRange := reqList.Ranges.Inside(int64(toIndex))
	logger = logger.With().Str("room", roomID).Int("from", fromIndex).Int("to", toIndex).Bool("inside_range", isInsideRange).Logger()
	logger.Info().Bool("newEvent", newEvent != nil).Msg("moved!")
	// the toIndex may not be inside a tracked range. If it isn't, we actually need to notify about a
	// different room
	if !isInsideRange {
		toIndex = int(reqList.Ranges.UpperClamp(int64(toIndex)))
		count := int(intList.Len())
		if toIndex >= count {
			// no room exists
			logger.Warn().Int("to", toIndex).Int("size", count).Msg(
				"cannot move to index, it's greater than the list of sorted rooms",
			)
			return nil
		}
		if toIndex == -1 {
			logger.Warn().Int("from", fromIndex).Int("to", toIndex).Interface("ranges", reqList.Ranges).Msg(
				"room moved but not in tracked ranges, ignoring",
			)
			return nil
		}
		toRoom := intList.Get(toIndex)

		// fake an update event for this room.
		// We do this because we are introducing a new room in the list because of this situation:
		// tracking [10,20] and room 24 jumps to position 0, so now we are tracking [9,19] as all rooms
		// have been shifted to the right, hence we need to inject a fake event for room 9 (client has 10-19)
		tempTimelineLimit := int(reqList.TimelineLimit)
		if tempTimelineLimit == 0 {
			// We need to make sure that we actually give a valid timeline limit here as we will yank the most
			// recent timeline event to inject as the fake event, hence min check
			tempTimelineLimit = 1
		}
		rooms := s.userCache.LazyLoadTimelines(s.loadPosition, []string{toRoom.RoomID}, tempTimelineLimit) // TODO: per-room timeline limit
		urd := rooms[toRoom.RoomID]

		// clobber before falling through
		roomID = toRoom.RoomID
		if len(urd.Timeline) > 0 {
			newEvent = urd.Timeline[len(urd.Timeline)-1]
		} else {
			logger.Warn().Str("to_room", toRoom.RoomID).Int("limit", tempTimelineLimit).Msg(
				"tried to lazy load timeline for room but no timeline entries were returned. " +
					"This isn't possible under normal operation, please report. " +
					"Rooms may be duplicated in the list.",
			)
			// do nothing and pretend the new event didn't exist...
			return nil
		}
	}

	return s.moveRoom(ctx, reqList, roomID, newEvent, fromIndex, toIndex, reqList.Ranges, isSubscribedToRoom, newlyAdded, forceInitial)
}

// Move a room from an absolute index position to another absolute position.
// 1,2,3,4,5
// 3 bumps to top -> 3,1,2,4,5 -> DELETE index=2, INSERT val=3 index=0
// 7 bumps to top -> 7,1,2,3,4 -> DELETE index=4, INSERT val=7 index=0
func (s *connStateLive) moveRoom(
	ctx context.Context,
	reqList *sync3.RequestList, roomID string, event json.RawMessage, fromIndex, toIndex int,
	ranges sync3.SliceRanges, onlySendRoomID, newlyAdded, forceInitial bool,
) []sync3.ResponseOp {
	if fromIndex == toIndex {
		// issue an UPDATE, nice and easy because we don't need to move entries in the list
		room := &sync3.Room{
			RoomID: roomID,
		}
		if newlyAdded || forceInitial {
			rooms := s.getInitialRoomData(ctx, reqList.RoomSubscription, roomID)
			room = &rooms[0]
		} else if !onlySendRoomID {
			room = s.getDeltaRoomData(roomID, event)
		}
		op := sync3.OpUpdate
		if newlyAdded {
			op = sync3.OpInsert
		}
		return []sync3.ResponseOp{
			&sync3.ResponseOpSingle{
				Operation: op,
				Index:     &fromIndex,
				Room:      room,
				RoomID:    roomID,
			},
		}
	}
	// work out which value to DELETE. This varies depending on where the room was and how much of the
	// list we are tracking. E.g moving to index=0 with ranges [0,99][100,199] and an update in
	// pos 150 -> DELETE 150, but if we weren't tracking [100,199] then we would DELETE 99. If we were
	// tracking [0,99][200,299] then it's still DELETE 99 as the 200-299 range isn't touched.
	deleteIndex := fromIndex
	if !ranges.Inside(int64(fromIndex)) {
		// we are not tracking this room, so no point issuing a DELETE for it. Instead, clamp the index
		// to the highest end-range marker < index
		deleteIndex = int(ranges.LowerClamp(int64(fromIndex)))
	}
	room := &sync3.Room{
		RoomID: roomID,
	}
	if !onlySendRoomID {
		rooms := s.getInitialRoomData(ctx, reqList.RoomSubscription, roomID)
		room = &rooms[0]
	}

	return []sync3.ResponseOp{
		&sync3.ResponseOpSingle{
			Operation: sync3.OpDelete,
			Index:     &deleteIndex,
		},
		&sync3.ResponseOpSingle{
			Operation: sync3.OpInsert,
			Index:     &toIndex,
			Room:      room,
			RoomID:    roomID,
		},
	}
}

func (s *ConnState) writeDeleteOp(reqList *sync3.RequestList, deletedIndex int) sync3.ResponseOp {
	// update operations return -1 if nothing gets deleted
	if deletedIndex < 0 {
		return nil
	}
	// only notify if we are tracking this index
	if !reqList.Ranges.Inside(int64(deletedIndex)) {
		return nil
	}
	return &sync3.ResponseOpSingle{
		Operation: sync3.OpDelete,
		Index:     &deletedIndex,
	}
}
