# speechmatics-punctuation-cleanup Specification

## Purpose
Keep Speechmatics captions readable when profanity or disfluency removal brings separate punctuation marks together, without rewriting unrelated punctuation or spoken-word timing.

## Requirements

### Requirement: Reconcile punctuation across removed words
When Speechmatics words tagged as profanity or disfluency are removed within one result message, the system SHALL reconcile collisions between separate comma or sentence-ending punctuation results brought together by that removal. Duplicate commas SHALL become one comma; a sentence ending SHALL take precedence over a comma; when both sides supply sentence endings, the existing ending SHALL be retained rather than appending another. These rules SHALL apply equally to either removal tag and to consecutive removed words. Punctuation with no existing conflicting punctuation SHALL continue to attach to the preceding surviving word.

#### Scenario: Duplicate commas across a disfluency
- **WHEN** the results represent `would , um , swim` with `um` tagged as disfluency
- **THEN** the emitted text is `would, swim`

#### Scenario: Duplicate commas across profanity
- **WHEN** the results represent `And , <profanity> , next` with the intervening word tagged as profanity
- **THEN** the emitted text is `And, next`

#### Scenario: Sentence ending replaces a comma
- **WHEN** the results represent `Okay , <removed> .` with the intervening word removed by either tag
- **THEN** the emitted text is `Okay.` and retains a sentence ending

#### Scenario: Existing sentence ending wins
- **WHEN** the results represent `Winston . <removed> , father` with the intervening word removed by either tag
- **THEN** the emitted text is `Winston. father`

#### Scenario: Two sentence endings meet
- **WHEN** the results represent `Really ? <removed> .` with the intervening word removed by either tag
- **THEN** the emitted text is `Really?`

#### Scenario: Consecutive filtered words
- **WHEN** the results represent `Well , <removed> , <removed> , anyway` with both intervening words tagged for removal
- **THEN** the emitted text is `Well, anyway`

#### Scenario: A single punctuation mark survives removal
- **WHEN** the results represent `well <removed> , anyway` with the intervening word tagged for removal
- **THEN** the emitted text remains `well, anyway`

### Requirement: Preserve punctuation outside removal collisions
The system SHALL leave punctuation unrelated to tagged-word removal unchanged, including abbreviation punctuation, ellipses, and question/exclamation combinations. Periods embedded in a word SHALL NOT be treated as a separate sentence-ending punctuation result for collision cleanup. Cleanup SHALL NOT rewrite previously published messages or flat-text fallback transcripts without result-level removal information.

#### Scenario: Legitimate abbreviation followed by a comma
- **WHEN** a surviving word contains `a.m.` and is followed by a comma punctuation result, either directly or after a removed word
- **THEN** the resulting text retains `a.m.,`

#### Scenario: Intentional punctuation without removal
- **WHEN** input contains `Wait...` or `Really?!` without a tagged-word removal separating the punctuation
- **THEN** that punctuation is preserved exactly

#### Scenario: Removal context does not leak past a kept word
- **WHEN** a removed word is followed by a surviving word and that word's ordinary punctuation
- **THEN** the punctuation following the surviving word is not normalized because of the earlier removal

### Requirement: Preserve transcript structure and delivery consistency
Cleanup SHALL preserve surviving words' start and end times and existing speaker grouping. It SHALL preserve existing segment start/duration bookkeeping, including punctuation-result end times even when a colliding mark is suppressed. It SHALL NOT emit punctuation-only transcripts when all words are removed or manufacture a preceding word for leading punctuation. Corrected word text SHALL be the source for both live caption events and saved transcript text.

#### Scenario: Timing and speaker attribution remain stable
- **WHEN** punctuation is reconciled around removed words in a message containing multiple speakers
- **THEN** surviving word timings and speaker grouping are unchanged and segment timing follows the existing punctuation attachment behavior

#### Scenario: Entire message removed
- **WHEN** every word in a result message is tagged for removal, with punctuation interspersed or trailing
- **THEN** no transcript is emitted

#### Scenario: Leading punctuation
- **WHEN** punctuation follows removed leading words before any surviving word
- **THEN** the punctuation is dropped rather than starting a transcript

#### Scenario: Live and saved text agree
- **WHEN** a corrected transcript ending in sentence punctuation is published
- **THEN** the live caption word text contains the corrected punctuation and the saved line uses the same correction and closes under the existing sentence-ending rules
