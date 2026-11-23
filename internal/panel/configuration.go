package panel

import "github.com/JoaoVictorVM/comporta/internal/config"

// Configuration shows the effective configuration with secrets masked and the
// status of the last reload. The source file is never read.
func (s *Service) Configuration() Configuration {
	snapshot := s.config.Capture()
	if snapshot == nil {
		return Configuration{}
	}
	status := s.config.Status()
	out := Configuration{Available: true, YAML: snapshot.EffectiveYAML(), Generation: status.ActiveGeneration, LastAttemptAt: status.LastAttemptAt,
		LastSuccessAt: status.LastSuccessAt, Result: string(status.Result), Rejected: status.Result == config.ResultRejected}
	for _, d := range status.Errors {
		out.Diagnostics = append(out.Diagnostics, Diagnostic{Code: string(d.Code), Path: d.Path, Message: d.Message})
	}
	return out
}
