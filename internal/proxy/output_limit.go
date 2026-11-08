package proxy

import "strconv"

func (p *prepared) capOutput(cap int64) error {
	if p.envelope.Operation != OperationGeneration || (p.envelope.MaxTokens.Known && p.envelope.MaxTokens.Value <= cap) {
		return nil
	}
	field := p.envelope.OutputLimitField
	if field == "" {
		field = "max_tokens"
	}
	patch, err := p.document.SetMember(p.document.Root(), field, []byte(strconv.FormatInt(cap, 10)))
	if err != nil {
		return err
	}
	p.patches = append(p.patches, patch)
	p.envelope.OutputLimitField = field
	p.envelope.MaxTokens = known(cap)
	p.refreshHashes()
	return nil
}
