package speaker

// Ready reports actual acquisition of the playback device. It does not change
// the audio path and does not claim that any queued audio has been heard.
func (p *Player) Ready() bool { pb, _ := p.device(); return pb != nil }
