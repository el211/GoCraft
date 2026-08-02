package handler

func sendCommandMessage(ctx CommandContext, text string) error {
	if ctx.Reply != nil {
		return ctx.Reply(text)
	}
	return sendSystemMessage(ctx.Conn, text)
}
