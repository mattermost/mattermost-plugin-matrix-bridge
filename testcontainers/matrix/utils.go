package matrix

// FindEventByPostID finds a Matrix event by its Mattermost post ID
func FindEventByPostID(events []Event, postID string) *Event {
	for _, event := range events {
		if mattermostPostID, exists := event.Content["mattermost_post_id"].(string); exists {
			if mattermostPostID == postID {
				return &event
			}
		}
	}
	return nil
}

// FindLatestMessageEvent finds the most recent m.room.message event
func FindLatestMessageEvent(events []Event) *Event {
	var latestEvent *Event
	var latestTimestamp int64

	for _, event := range events {
		if event.Type == "m.room.message" {
			if event.Timestamp > latestTimestamp {
				latestTimestamp = event.Timestamp
				latestEvent = &event
			}
		}
	}

	return latestEvent
}

// FindEventByType finds the first event of a specific type
func FindEventByType(events []Event, eventType string) *Event {
	for _, event := range events {
		if event.Type == eventType {
			return &event
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
func GetEventContent(event Event) map[string]any {
	return event.Content
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
