package translator

import (
	"bufio"
	_ "embed"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"
)

// These are the two dictionaries used by OpenCC's t2s configuration. The
// files are redistributed under Apache-2.0; see dictionary/LICENSE.
//
//go:embed dictionary/TSPhrases.txt
var traditionalPhraseDictionary string

//go:embed dictionary/TSCharacters.txt
var traditionalCharacterDictionary string

var (
	t2sOnce      sync.Once
	t2sConverter *traditionalToSimplified
	t2sInitErr   error
)

type dictionaryNode struct {
	children    map[rune]*dictionaryNode
	replacement string
}

// traditionalToSimplified is an immutable longest-prefix dictionary matcher.
// It deliberately replaces the previous OpenCC Go wrapper because that
// wrapper's matcher pulled GPLv2 code into the production binary.
type traditionalToSimplified struct {
	root        dictionaryNode
	maxKeyRunes int
}

func traditionalToSimplifiedConverter() (*traditionalToSimplified, error) {
	t2sOnce.Do(func() {
		t2sConverter, t2sInitErr = newTraditionalToSimplified(
			traditionalPhraseDictionary,
			traditionalCharacterDictionary,
		)
	})
	return t2sConverter, t2sInitErr
}

func newTraditionalToSimplified(dictionaries ...string) (*traditionalToSimplified, error) {
	converter := &traditionalToSimplified{root: dictionaryNode{children: make(map[rune]*dictionaryNode)}}
	for _, dictionary := range dictionaries {
		if err := converter.addDictionary(dictionary); err != nil {
			return nil, err
		}
	}
	if converter.maxKeyRunes == 0 {
		return nil, fmt.Errorf("t2s dictionary is empty")
	}
	return converter, nil
}

func (c *traditionalToSimplified) addDictionary(dictionary string) error {
	scanner := bufio.NewScanner(strings.NewReader(dictionary))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		source, targets, ok := strings.Cut(line, "\t")
		if !ok || source == "" {
			return fmt.Errorf("invalid t2s dictionary entry on line %d", lineNumber)
		}
		targetOptions := strings.Fields(targets)
		if len(targetOptions) == 0 {
			return fmt.Errorf("empty t2s dictionary target on line %d", lineNumber)
		}
		c.add(source, targetOptions[0])
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read t2s dictionary: %w", err)
	}
	return nil
}

func (c *traditionalToSimplified) add(source, replacement string) {
	node := &c.root
	for _, r := range source {
		if node.children == nil {
			node.children = make(map[rune]*dictionaryNode)
		}
		child := node.children[r]
		if child == nil {
			child = &dictionaryNode{}
			node.children[r] = child
		}
		node = child
	}
	// Phrase mappings are loaded first and retain priority for duplicate keys.
	if node.replacement == "" {
		node.replacement = replacement
	}
	c.maxKeyRunes = max(c.maxKeyRunes, utf8.RuneCountInString(source))
}

func (c *traditionalToSimplified) Convert(text string) (string, error) {
	if c == nil || c.maxKeyRunes == 0 {
		return "", fmt.Errorf("t2s converter is not initialized")
	}
	runes := []rune(text)
	var output strings.Builder
	output.Grow(len(text))
	for index := 0; index < len(runes); {
		node := &c.root
		replacement := ""
		matchedRunes := 0
		limit := min(len(runes), index+c.maxKeyRunes)
		for cursor := index; cursor < limit; cursor++ {
			node = node.children[runes[cursor]]
			if node == nil {
				break
			}
			if node.replacement != "" {
				replacement = node.replacement
				matchedRunes = cursor - index + 1
			}
		}
		if matchedRunes == 0 {
			output.WriteRune(runes[index])
			index++
			continue
		}
		output.WriteString(replacement)
		index += matchedRunes
	}
	return output.String(), nil
}
