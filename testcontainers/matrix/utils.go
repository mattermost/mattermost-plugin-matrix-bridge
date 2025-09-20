package matrix

// FindEventByPostID finds a Matrix event by its Mattermost post ID
func FindEventByPostID(events []Event, postID string) *Event {
	for i, event := range events {
		if mattermostPostID, exists := event.Content["mattermost_post_id"].(string); exists {
			if mattermostPostID == postID {
				return &events[i]
			}
		}
	}
	return nil
}

// FindLatestMessageEvent finds the most recent m.room.message event
func FindLatestMessageEvent(events []Event) *Event {
	var latestEventIndex = -1
	var latestTimestamp int64

	for i, event := range events {
		if event.Type == "m.room.message" {
			if event.Timestamp > latestTimestamp {
				latestTimestamp = event.Timestamp
				latestEventIndex = i
			}
		}
	}

	if latestEventIndex >= 0 {
		return &events[latestEventIndex]
	}
	return nil
}

// FindEventByType finds the first event of a specific type
func FindEventByType(events []Event, eventType string) *Event {
	for i, event := range events {
		if event.Type == eventType {
			return &events[i]
		}
	}
	return nil
}

// FindEventsByType finds all events of a specific type
func FindEventsByType(events []Event, eventType string) []Event {
	var result []Event
	for _, event := range events {
		if event.Type == eventType {
			result = append(result, event)
		}
	}
	return result
}

// GetEventContent extracts content from a Matrix event
// Returns the content map and a boolean indicating if content exists
func GetEventContent(event Event) (map[string]any, bool) {
	if event.Content != nil {
		return event.Content, true
	}
	return nil, false
}

// GetEventSender extracts the sender from a Matrix event
func GetEventSender(event Event) string {
	return event.Sender
}

// GetEventID extracts the event ID from a Matrix event
func GetEventID(event Event) string {
	return event.EventID
}

// GetRoomID extracts the room ID from a Matrix event
func GetRoomID(event Event) string {
	return event.RoomID
}
