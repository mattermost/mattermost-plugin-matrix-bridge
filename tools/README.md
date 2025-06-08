# Tools

This directory contains utility tools for the Mattermost-Matrix bridge plugin.

## Emoji Generator

The `emoji_generator.go` tool downloads the latest emoji mappings from Mattermost's source code and generates a comprehensive Go file with all emoji-to-Unicode conversions.

### Usage

```bash
cd tools
go run emoji_generator.go ../server/emoji_mappings_generated.go
```

### What it does

1. Downloads the emoji mapping file from: `https://raw.githubusercontent.com/mattermost/mattermost/master/webapp/channels/src/utils/emoji.ts`
2. Parses the `EmojiIndicesByAlias` map (emoji names to indices)
3. Parses the `EmojiIndicesByUnicode` map (indices to Unicode hex codes)
4. Generates a Go file with two maps:
   - `emojiNameToIndex`: Maps emoji names like "smile" to internal indices
   - `emojiIndexToUnicode`: Maps indices to Unicode hex strings like "1f604"

### Output

The generated file contains:
- **~4,400+ emoji aliases** covering the complete Mattermost emoji set
- **~3,300+ Unicode mappings** for proper emoji rendering
- Support for complex emoji sequences with variation selectors and zero-width joiners
- Automatic handling of skin tone variations and composite emojis

### When to regenerate

Run this tool when:
- Mattermost updates their emoji set
- New Unicode emoji standards are adopted
- The emoji conversion is missing specific emojis

### Dependencies

- Go (for running the generator)
- Internet connection (to download the latest Mattermost emoji data)

The generated file replaces `server/emoji_mappings_generated.go` and provides comprehensive emoji conversion capability for the Matrix bridge.