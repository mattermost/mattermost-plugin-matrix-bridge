package main

import (
	"testing"
)

func TestParseDisplayName(t *testing.T) {
	// Create a mock plugin instance for testing
	plugin := &Plugin{}

	tests := []struct {
		name              string
		displayName       string
		expectedFirstName string
		expectedLastName  string
	}{
		{
			name:              "empty string",
			displayName:       "",
			expectedFirstName: "",
			expectedLastName:  "",
		},
		{
			name:              "whitespace only",
			displayName:       "   \t\n   ",
			expectedFirstName: "",
			expectedLastName:  "",
		},
		{
			name:              "single name",
			displayName:       "John",
			expectedFirstName: "John",
			expectedLastName:  "",
		},
		{
			name:              "single name with whitespace",
			displayName:       "  Alice  ",
			expectedFirstName: "Alice",
			expectedLastName:  "",
		},
		{
			name:              "first and last name",
			displayName:       "John Doe",
			expectedFirstName: "John",
			expectedLastName:  "Doe",
		},
		{
			name:              "first and last name with extra whitespace",
			displayName:       "  John   Doe  ",
			expectedFirstName: "John",
			expectedLastName:  "Doe",
		},
		{
			name:              "three names",
			displayName:       "John Michael Doe",
			expectedFirstName: "John",
			expectedLastName:  "Michael Doe",
		},
		{
			name:              "multiple names",
			displayName:       "Mary Jane Watson Parker",
			expectedFirstName: "Mary",
			expectedLastName:  "Jane Watson Parker",
		},
		{
			name:              "name with prefix/suffix",
			displayName:       "Dr. John von Neumann Jr.",
			expectedFirstName: "Dr.",
			expectedLastName:  "John von Neumann Jr.",
		},
		{
			name:              "unicode characters",
			displayName:       "José María García",
			expectedFirstName: "José",
			expectedLastName:  "María García",
		},
		{
			name:              "single unicode name",
			displayName:       "李小明",
			expectedFirstName: "李小明",
			expectedLastName:  "",
		},
		{
			name:              "names with hyphen",
			displayName:       "Mary-Jane Watson",
			expectedFirstName: "Mary-Jane",
			expectedLastName:  "Watson",
		},
		{
			name:              "names with apostrophe",
			displayName:       "John O'Connor",
			expectedFirstName: "John",
			expectedLastName:  "O'Connor",
		},
		{
			name:              "single character names",
			displayName:       "A B",
			expectedFirstName: "A",
			expectedLastName:  "B",
		},
		{
			name:              "very long name",
			displayName:       "Jean-Baptiste Grenouille de la Montagne",
			expectedFirstName: "Jean-Baptiste",
			expectedLastName:  "Grenouille de la Montagne",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			firstName, lastName := plugin.parseDisplayName(tt.displayName)

			if firstName != tt.expectedFirstName {
				t.Errorf("parseDisplayName(%q) firstName = %q, want %q",
					tt.displayName, firstName, tt.expectedFirstName)
			}

			if lastName != tt.expectedLastName {
				t.Errorf("parseDisplayName(%q) lastName = %q, want %q",
					tt.displayName, lastName, tt.expectedLastName)
			}
		})
	}
}

func TestParseDisplayNameEdgeCases(t *testing.T) {
	plugin := &Plugin{}

	// Test with tabs and multiple spaces
	firstName, lastName := plugin.parseDisplayName("John\t\t   Doe")
	if firstName != "John" || lastName != "Doe" {
		t.Errorf("parseDisplayName with tabs failed: got (%q, %q), want (\"John\", \"Doe\")", firstName, lastName)
	}

	// Test with newlines
	firstName, lastName = plugin.parseDisplayName("John\nDoe")
	if firstName != "John" || lastName != "Doe" {
		t.Errorf("parseDisplayName with newlines failed: got (%q, %q), want (\"John\", \"Doe\")", firstName, lastName)
	}

	// Test with mixed whitespace
	firstName, lastName = plugin.parseDisplayName(" \t John \n  Michael \r  Doe \t ")
	if firstName != "John" || lastName != "Michael Doe" {
		t.Errorf("parseDisplayName with mixed whitespace failed: got (%q, %q), want (\"John\", \"Michael Doe\")", firstName, lastName)
	}
}
