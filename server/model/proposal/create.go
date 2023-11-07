package proposal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"plandex-server/model"
	"plandex-server/model/lib"
	"plandex-server/types"

	"github.com/google/uuid"
	"github.com/plandex/plandex/shared"
	"github.com/sashabaranov/go-openai"
)

const systemMessageHead = shared.IdentityPrompt + ` A plan is a set of files with an attached context.` +

	"Your instructions:\n\n```\n" +

	`Based on user-provided context, create a plan for the task using the following steps:

		1. Decide whether you've been given enough information and context to make a good plan. 
			a. If not:
		    - Explicitly say "I need more information or context to make a plan for this task."
			  - Ask the user for more information or context and stop there.

		2. Decide whether this task is small enough to be completed in a single response.
			a. If so, write out the code to complete the task. Include only lines that will change and lines that are necessary to know where the changes should be applied. Precede the code block with the file path like this '- file_path:'--for example:
				- src/main.rs:				
				- lib/utils.go:
				- main.py:
				File paths should always come *before* the opening triple backticks of a code block. They should *not* be included in the code block itself.
				File paths should appear *immediately* before the opening triple backticks of a code block. There should be *no other lines* between the file path and the code block. Any explanations should come either *before the file path or after the code block.*
			b. If not: 
			  - Explicitly say "I will break this large task into subtasks."
				- Divide the task into smaller subtasks and list them in a numbered list. Stop there.
		
		Always precede code blocks the file path as described above in 2a. Code must *always* be labelled with the path. 
		
		Every file you reference should either exist in the context directly or be a new file that will be created in the same base directory a file in the context. For example, if there is a file in context at path 'lib/utils.go', you can create a new file at path 'lib/utils_test.go' but *not* at path 'src/lib/utils.go'.

		For code in markdown blocks, always include the language name after the opening triple backticks.
		
		Don't include unnecessary comments in code. Lean towards no comments as much as you can. If you must include a comment to make the code understandable, be sure it is concise. Don't use comments to communicate with the user.

		An exception to the above instructions on comments are if a file block is empty because you removed everything in it. In that case, leave a brief one-line comment starting with 'Plandex: removed' that says what was removed so that the file block isn't empty.

		In code blocks, include the *minimum amount of code* necessary to describe the suggested changes. Include only lines that are changing and and lines that make it clear where the change should be applied. You can use comments like "// rest of the function..." or "// rest of the file..." to help make it clear where changes should be applied. You *must not* include large sections of the original file unless it helps make the suggested changes clear.

		At the end of a plan, you can suggest additional iterations to make the plan better. You can also ask the user to load more files or information into context if it would help you make a better plan.` +
	"\n```\n\n" +
	"User-provided context:"

var systemHeadNumTokens = shared.GetNumTokens(systemMessageHead)

const promptWrapperFormatStr = "The user's latest prompt:\n```\n%s\n```\n\n Please respond according to the 'Your instructions' section above. Remember to precede code blocks with the file path *exactly* as described in 2a. Do not use any other formatting for file paths."

var promptWrapperTokens = shared.GetNumTokens(fmt.Sprintf(promptWrapperFormatStr, ""))

// Proposal function to create a new proposal
func CreateProposal(req shared.PromptRequest, onStream types.OnStreamFunc) error {
	goEnv := os.Getenv("GOENV") // Fetch the GO_ENV environment variable

	fmt.Println("GOENV: " + goEnv)
	if goEnv == "test" {
		streamLoremIpsum(onStream)
		return nil
	}

	proposalUUID, err := uuid.NewRandom()
	if err != nil {
		fmt.Printf("Failed to generate proposal id: %v\n", err)
		return err
	}
	proposalId := proposalUUID.String()

	ctx, cancel := context.WithCancel(context.Background())

	rootId := req.RootProposalId
	if rootId == "" {
		rootId = proposalId
	}
	proposal := types.Proposal{
		Id:       proposalId,
		ParentId: req.ParentProposalId,
		IsRoot:   req.ParentProposalId == "",
		RootId:   rootId,
		Request:  &req,
		Content:  "",
		ProposalStage: types.ProposalStage{
			CancelFn: &cancel,
		},
	}

	onStream(proposalId, nil)

	contextText, contextTokens := lib.FormatModelContext(req.ModelContext)
	systemMessageText := systemMessageHead + contextText
	systemMessage := openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleSystem,
		Content: systemMessageText,
	}

	messages := []openai.ChatCompletionMessage{
		systemMessage,
	}

	promptTokens := promptWrapperTokens + shared.GetNumTokens(req.Prompt)
	totalTokens := systemHeadNumTokens + contextTokens + promptTokens

	// print out breakdown of token usage
	fmt.Printf("System message tokens: %d\n", systemHeadNumTokens)
	fmt.Printf("Context tokens: %d\n", contextTokens)
	fmt.Printf("Prompt tokens: %d\n", promptTokens)
	fmt.Printf("Total tokens before convo: %d\n", totalTokens)

	if totalTokens > shared.MaxTokens {
		// token limit already exceeded before adding conversation
		err := fmt.Errorf("token limit exceeded before adding conversation")
		fmt.Printf("Error: %v\n", err)
		return err
	}

	conversationTokens := 0
	tokensUpToTimestamp := make(map[string]int)
	for _, convoMessage := range req.Conversation {
		conversationTokens += convoMessage.Tokens
		tokensUpToTimestamp[convoMessage.Timestamp] = conversationTokens
		// fmt.Printf("Timestamp: %s | Tokens: %d | Total: %d | conversationTokens\n", convoMessage.Timestamp, convoMessage.Tokens, conversationTokens)
	}

	fmt.Printf("Conversation tokens: %d\n", conversationTokens)

	var summary *shared.ConversationSummary
	if (totalTokens + conversationTokens) > shared.MaxTokens {
		fmt.Println("Token limit exceeded. Attempting to reduce via conversation summary.")

		// token limit exceeded after adding conversation
		// get summary for as much as the conversation as necessary to stay under the token limit
		for _, s := range req.ConversationSummaries {
			tokens, ok := tokensUpToTimestamp[s.LastMessageTimestamp]

			fmt.Printf("Last message timestamp: %s\n", s.LastMessageTimestamp)

			if !ok {
				err := fmt.Errorf("conversation summary timestamp not found in conversation")
				fmt.Printf("Error: %v\n", err)
				return err
			}

			updatedConversationTokens := (conversationTokens - tokens) + s.Tokens
			savedTokens := conversationTokens - updatedConversationTokens

			fmt.Printf("Conversation summary tokens: %d\n", tokens)
			fmt.Printf("Updated conversation tokens: %d\n", updatedConversationTokens)
			fmt.Printf("Saved tokens: %d\n", savedTokens)

			if (totalTokens + updatedConversationTokens) <= shared.MaxTokens {
				fmt.Printf("Summarizing up to %s | saving %d tokens\n", s.LastMessageTimestamp, savedTokens)
				summary = &s
				break
			}
		}

		if summary == nil {
			err := errors.New("couldn't get under token limit with conversation summary")
			fmt.Printf("Error: %v\n", err)
			return err
		}
	}

	if summary == nil {
		for _, convoMessage := range req.Conversation {
			messages = append(messages, convoMessage.Message)
		}
	} else {
		if (totalTokens + summary.Tokens) > shared.MaxTokens {
			err := fmt.Errorf("token limit still exceeded after summarizing conversation")
			return err
		}

		messages = append(messages, openai.ChatCompletionMessage{
			Role:    openai.ChatMessageRoleAssistant,
			Content: summary.Summary,
		})

		// add messages after the last message in the summary
		for _, convoMessage := range req.Conversation {
			if convoMessage.Timestamp > summary.LastMessageTimestamp {
				messages = append(messages, convoMessage.Message)
			}
		}
	}

	promptMessage := openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleUser,
		Content: fmt.Sprintf(promptWrapperFormatStr, req.Prompt),
	}

	messages = append(messages, promptMessage)

	// fmt.Println("\n\nMessages:")
	// for _, message := range messages {
	// 	fmt.Printf("%s: %s\n", message.Role, message.Content)
	// }

	// store the proposal
	proposals.Set(proposalId, &proposal)

	replyInfo := shared.NewReplyInfo()

	modelReq := openai.ChatCompletionRequest{
		Model:    model.StrongModel,
		Messages: messages,
		Stream:   true,
	}

	stream, err := model.Client.CreateChatCompletionStream(ctx, modelReq)
	if err != nil {
		fmt.Printf("Error creating proposal GPT4 stream: %v\n", err)
		fmt.Println(err)

		errStr := err.Error()
		if strings.Contains(errStr, "status code: 400") &&
			strings.Contains(errStr, "reduce the length of the messages") {
			fmt.Println("Token limit exceeded")
		}

		proposals.Delete(proposalId)
		return err
	}

	onError := func(err error) {
		fmt.Printf("\nStream error: %v\n", err)

		proposals.Update(proposalId, func(proposal *types.Proposal) {
			proposal.SetErr(err)
		})
		onStream("", err)
	}

	go func() {
		defer stream.Close()

		// Create a timer that will trigger if no chunk is received within the specified duration
		timer := time.NewTimer(model.OPENAI_STREAM_CHUNK_TIMEOUT)
		defer timer.Stop()

		for {
			select {
			case <-ctx.Done():
				// The main context was canceled (not the timer)
				return
			case <-timer.C:
				// Timer triggered because no new chunk was received in time
				fmt.Println("\nStream timeout due to inactivity")
				onError(fmt.Errorf("stream timeout due to inactivity"))
				return
			default:
				response, err := stream.Recv()

				if err == nil {
					// Successfully received a chunk, reset the timer
					if !timer.Stop() {
						<-timer.C
					}
					timer.Reset(model.OPENAI_STREAM_CHUNK_TIMEOUT)
				}

				if err != nil {
					onError(fmt.Errorf("stream error: %v", err))
					return
				}

				if len(response.Choices) == 0 {
					onError(fmt.Errorf("stream finished with no choices"))
					return
				}

				if len(response.Choices) > 1 {
					onError(fmt.Errorf("stream finished with more than one choice"))
					return
				}

				choice := response.Choices[0]

				if choice.FinishReason != "" {
					onStream(shared.STREAM_DESCRIPTION_PHASE, nil)
					responseTs := shared.StringTs()

					if len(req.Conversation) > 0 {
						summaryCh := make(chan *shared.ConversationSummary)
						errCh := make(chan error)

						summaryProc := types.ConvoSummaryProc{
							SummaryCh: summaryCh,
							ErrCh:     errCh,
						}

						convoSummaryProcs.Set(proposal.RootId, &summaryProc)

						go func() {
							fmt.Println("Generating plan summary for rootId:", proposal.RootId)

							defer func() {
								close(summaryCh)
								close(errCh)
							}()

							var summaryMessages []openai.ChatCompletionMessage
							var latestSummary *shared.ConversationSummary
							var numMessagesSummarized int = 0
							if len(req.ConversationSummaries) > 0 {
								latestSummary = &req.ConversationSummaries[len(req.ConversationSummaries)-1]
								numMessagesSummarized = latestSummary.NumMessages
							}

							if latestSummary == nil {
								for _, convoMessage := range req.Conversation {
									summaryMessages = append(summaryMessages, convoMessage.Message)
								}
							} else {
								summaryMessages = append(summaryMessages, openai.ChatCompletionMessage{
									Role:    openai.ChatMessageRoleAssistant,
									Content: latestSummary.Summary,
								})
							}

							summaryMessages = append(summaryMessages, promptMessage, openai.ChatCompletionMessage{
								Role:    openai.ChatMessageRoleAssistant,
								Content: proposal.Content,
							})

							summary, err := model.PlanSummary(summaryMessages, responseTs, numMessagesSummarized+1)
							if err != nil {
								fmt.Printf("Error generating plan summary for root %s: %v\n", proposal.RootId, err)

								convoSummaryProcs.Update(proposal.RootId, func(proc *types.ConvoSummaryProc) {
									proc.Err = err
								})
								errCh <- err
								return
							}

							fmt.Println("Generated plan summary for root", proposal.RootId)

							convoSummaries.Set(proposal.RootId, summary)
							summaryCh <- summary

						}()
					}

					files, fileContents, numTokensByFile, _ := replyInfo.FinishAndRead()

					var planDescription *shared.PlanDescription

					if len(files) == 0 {
						planDescription = &shared.PlanDescription{
							Files:             []string{},
							MadePlan:          false,
							ResponseTimestamp: responseTs,
						}
					} else {
						planDescription, err = genPlanDescriptionJson(proposalId, ctx)
						if err != nil {
							onError(fmt.Errorf("failed to generate plan description json: %v", err))
							return
						}

						planDescription.MadePlan = true
						planDescription.Files = files
						planDescription.ResponseTimestamp = responseTs
					}

					if summary != nil {
						planDescription.SummarizedToTimestamp = summary.LastMessageTimestamp
					}

					bytes, err := json.Marshal(planDescription)
					if err != nil {
						onError(fmt.Errorf("failed to marshal plan description: %v", err))
						return
					}
					planDescriptionJson := string(bytes)

					fmt.Println("Plan description json:")
					fmt.Println(planDescriptionJson)

					proposals.Update(proposalId, func(proposal *types.Proposal) {
						proposal.Finish(planDescription)
					})

					onStream(planDescriptionJson, nil)

					if len(files) == 0 {
						onStream(shared.STREAM_FINISHED, nil)
					} else {
						onStream(shared.STREAM_BUILD_PHASE, nil)
						err = buildPlan(proposalId, fileContents, numTokensByFile, onStream)
						if err != nil {
							onError(fmt.Errorf("failed to confirm proposal: %v", err))
						}
					}
					return
				}

				delta := choice.Delta
				content := delta.Content
				proposals.Update(proposalId, func(proposal *types.Proposal) {
					proposal.Content += content
				})

				// fmt.Printf("%s", content)
				onStream(content, nil)
				replyInfo.AddToken(content, true)

			}
		}
	}()

	return nil
}
