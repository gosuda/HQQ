package conformance

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"

	"gosuda.org/hqq"
)

// goldenSeed is the payload seed of the one message of the golden sequence.
const goldenSeed = 1

// goldenStepNames and goldenStepActions describe the six snapshots of G-06,
// in order. Each snapshot is taken after its action.
var goldenStepNames = [...]struct{ name, action string }{
	{"1-primary-ready", "a Primary creates the segment on caller memory (heartbeats disabled)"},
	{"2-secondary-joined", "a Secondary joins as the second endpoint"},
	{"3-message-in-flight", "the Primary writes one message (Payload(1, 0, bufferSize)); the Secondary has not read it"},
	{"4-message-consumed", "the Secondary reads the message"},
	{"5-both-closed", "the Primary and the Secondary close (both detach)"},
	{"6-reinitialized", "a new Primary re-initializes the dead segment (epoch 2)"},
}

// runGoldenSequence runs the six steps of G-06 on mem, which must hold a
// segment of geometry count x size, and calls snap(i) after step i. It is the
// single definition of the sequence: the generator records it and the replay
// test runs it again.
func runGoldenSequence(mem []byte, count, size int, snap func(step int) error) error {
	opts := hqq.LinkOptions{HeartbeatInterval: -1}
	prim, err := hqq.NewStandardLink(mem, count, size, opts)
	if err != nil {
		return fmt.Errorf("step 1: %w", err)
	}
	if err := snap(0); err != nil {
		return err
	}
	sec, err := hqq.NewStandardLink(mem, count, size, opts)
	if err != nil {
		return fmt.Errorf("step 2: %w", err)
	}
	if err := snap(1); err != nil {
		return err
	}
	msg := Payload(goldenSeed, 0, size)
	if _, err := prim.Write(msg); err != nil {
		return fmt.Errorf("step 3: %w", err)
	}
	if err := snap(2); err != nil {
		return err
	}
	got := make([]byte, size)
	n, err := sec.Read(got)
	if err != nil {
		return fmt.Errorf("step 4: %w", err)
	}
	if !bytes.Equal(got[:n], msg) {
		return fmt.Errorf("step 4: read %d bytes that differ from the message", n)
	}
	if err := snap(3); err != nil {
		return err
	}
	if err := prim.Close(); err != nil {
		return fmt.Errorf("step 5: %w", err)
	}
	if err := sec.Close(); err != nil {
		return fmt.Errorf("step 5: %w", err)
	}
	if err := snap(4); err != nil {
		return err
	}
	re, err := hqq.NewStandardLink(mem, count, size, opts)
	if err != nil {
		return fmt.Errorf("step 6: %w", err)
	}
	defer re.Close()
	return snap(5)
}

// goldenSequences runs G-06 on caller memory with the host page size and
// adds the snapshots to files and the description to sequences/<page>/.
// It also checks that the first snapshot equals the layout model's image of
// a Primary (the cross-check of the model against the Go implementation).
func goldenSequences(files map[string][]byte) error {
	page := os.Getpagesize()
	total := int(hqq.SizeStandardLink(sbCount, sbSize))
	if total == 0 {
		return fmt.Errorf("conformance: Go reports no layout for %d x %d", sbCount, sbSize)
	}
	mem := hqq.AlignedBuffer(uintptr(total))
	seq := GoldenSequence{
		PageSize:    page,
		BufferCount: sbCount,
		BufferSize:  sbSize,
		LayoutSize:  total,
		Seed:        goldenSeed,
	}
	var snaps [][]byte
	err := runGoldenSequence(mem, sbCount, sbSize, func(step int) error {
		img := append([]byte(nil), mem...)
		words := normalizeSegment(img)
		snaps = append(snaps, img)
		seq.Steps = append(seq.Steps, SequenceStep{
			Name:       goldenStepNames[step].name,
			Action:     goldenStepNames[step].action,
			File:       familyPath("sequences", page, goldenStepNames[step].name+".bin"),
			Epoch:      binary.LittleEndian.Uint64(img[SBEpoch:]),
			Normalized: words,
		})
		return nil
	})
	if err != nil {
		return err
	}
	model, err := modelPrimaryReady(page, sbCount, sbSize)
	if err != nil {
		return err
	}
	if !bytes.Equal(snaps[0], model) {
		return fmt.Errorf("conformance: layout model disagrees with the Go implementation at step 1 (page %d)", page)
	}
	for i, st := range seq.Steps {
		if err := putFile(files, st.File, snaps[i]); err != nil {
			return err
		}
	}
	doc, err := json.MarshalIndent(seq, "", "  ")
	if err != nil {
		return err
	}
	return putFile(files, familyPath("sequences", page, "sequence.json"), append(doc, '\n'))
}
