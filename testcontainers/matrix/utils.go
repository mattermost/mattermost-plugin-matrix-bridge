package matrix

// FindEventByPostID finds a Matrix event by its Mattermost post ID
func FindEventByPostID(events []map[string]any, postID string) map[string]any {
	for _, event := range events {
		if content, ok := event["content"].(map[string]any); ok {
			if mattermostPostID, exists := content["mattermost_post_id"].(string); exists {
				if mattermostPostID == postID {
					return event
				}
			}
		}
	}
	return nil
}

// FindLatestMessageEvent finds the most recent m.room.message event
func FindLatestMessageEvent(events []map[string]any) map[string]any {
	var latestEvent map[string]any
	var latestTimestamp float64

	for _, event := range events {
		if event["type"] == "m.room.message" {
			if timestamp, ok := event["origin_server_ts"].(float64); ok {
				if timestamp > latestTimestamp {
					latestTimestamp = timestamp
					latestEvent = event
				}
			}
		}
	}

	return latestEvent
}

// FindEventByType finds the first event of a specific type
func FindEventByType(events []map[string]any, eventType string) map[string]any {
	for _, event := range events {
		if event["type"] == eventType {
			return event
		}
	}
	return nil
}

// FindEventsByType finds all events of a specific type
func FindEventsByType(events []map[string]any, eventType string) []map[string]any {
	var result []map[string]any
	for _, event := range events {
		if event["type"] == eventType {
			result = append(result, event)
		}
	}
	return result
}

// GetEventContent extracts and validates content from a Matrix event
func GetEventContent(event map[string]any) (map[string]any, bool) {
	content, ok := event["content"].(map[string]any)
	return content, ok
}

// GetEventSender extracts the sender from a Matrix event
func GetEventSender(event map[string]any) (string, bool) {
	sender, ok := event["sender"].(string)
	return sender, ok
}

// GetEventID extracts the event ID from a Matrix event
func GetEventID(event map[string]any) (string, bool) {
	eventID, ok := event["event_id"].(string)
	return eventID, ok
}

// GetRoomID extracts the room ID from a Matrix event
func GetRoomID(event map[string]any) (string, bool) {
	roomID, ok := event["room_id"].(string)
	return roomID, ok
}
