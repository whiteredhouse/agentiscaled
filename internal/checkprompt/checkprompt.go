// Package checkprompt holds the prompts of runtime check's turns, shared by
// the check and the fake ACP agent its tests run against.
package checkprompt

const (
	// Done asks for a turn that simply completes.
	Done = "Reply with the single word ready. Do not use any tools."
	// Question asks the agent to ask one question and remember a word.
	Question = "Before anything else, ask the sender which color they like, as your one question. Remember the word lantern. After they answer, reply with both that word and their color. Do not use any tools."
	// Answer answers Question's question.
	Answer = "Blue."
	// Long asks for a turn long enough to cancel.
	Long = "Count from 1 to 100000, one number per line. Do not stop until you are done."
)
