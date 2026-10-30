package devices

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"testing"
)

func decodeHexOrPanic(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

func TestTestVectorAEndToEnd(t *testing.T) {
	// 1. Ephemeral X25519 Key Agreement
	ecBytes := decodeHexOrPanic("1111111111111111111111111111111111111111111111111111111111111111")
	ehBytes := decodeHexOrPanic("2222222222222222222222222222222222222222222222222222222222222222")

	clientPair, err := EphemeralKeyPairFromScalar(ecBytes)
	if err != nil {
		t.Fatalf("client EphemeralKeyPairFromScalar failed: %v", err)
	}
	hostPair, err := EphemeralKeyPairFromScalar(ehBytes)
	if err != nil {
		t.Fatalf("host EphemeralKeyPairFromScalar failed: %v", err)
	}

	expectedEc := "7b4e909bbe7ffe44c465a220037d608ee35897d31ef972f07f74892cb0f73f13"
	if hex.EncodeToString(clientPair.PublicKey) != expectedEc {
		t.Errorf("Ec = %s, want %s", hex.EncodeToString(clientPair.PublicKey), expectedEc)
	}

	expectedEh := "0faa684ed28867b97f4a6a2dee5df8ce974e76b7018e3f22a1c4cf2678570f20"
	if hex.EncodeToString(hostPair.PublicKey) != expectedEh {
		t.Errorf("Eh = %s, want %s", hex.EncodeToString(hostPair.PublicKey), expectedEh)
	}

	kClient, err := ComputeSharedSecret(clientPair.PrivateKey, hostPair.PublicKey)
	if err != nil {
		t.Fatalf("ComputeSharedSecret client failed: %v", err)
	}
	kHost, err := ComputeSharedSecret(hostPair.PrivateKey, clientPair.PublicKey)
	if err != nil {
		t.Fatalf("ComputeSharedSecret host failed: %v", err)
	}

	if hex.EncodeToString(kClient) != hex.EncodeToString(kHost) {
		t.Fatalf("shared secrets do not match")
	}
	expectedK := "9e004098efc091d4ec2663b4e9f5cfd4d7064571690b4bea97ab146ab9f35056"
	if hex.EncodeToString(kClient) != expectedK {
		t.Errorf("K_shared = %s, want %s", hex.EncodeToString(kClient), expectedK)
	}

	// 2. Commitments & Salts
	rc := decodeHexOrPanic("44444444444444444444444444444444")
	rh := decodeHexOrPanic("55555555555555555555555555555555")

	commitC, err := ComputeCommitment(clientPair.PublicKey, rc)
	if err != nil {
		t.Fatalf("ComputeCommitment client failed: %v", err)
	}
	commitH, err := ComputeCommitment(hostPair.PublicKey, rh)
	if err != nil {
		t.Fatalf("ComputeCommitment host failed: %v", err)
	}

	expectedCommitC := "ec64eaa50a2dc6af146358910342adb07e73dd599ae341cfd7aeb623c5222538"
	if hex.EncodeToString(commitC[:]) != expectedCommitC {
		t.Errorf("Commit_c = %s, want %s", hex.EncodeToString(commitC[:]), expectedCommitC)
	}

	expectedCommitH := "5b4053fb5ca507626b397f5f909f75eb6a9c0870a60eb83dfd8bd9d5790a299e"
	if hex.EncodeToString(commitH[:]) != expectedCommitH {
		t.Errorf("Commit_h = %s, want %s", hex.EncodeToString(commitH[:]), expectedCommitH)
	}

	if !VerifyCommitment(commitC[:], clientPair.PublicKey, rc) {
		t.Errorf("VerifyCommitment client returned false")
	}
	if !VerifyCommitment(commitH[:], hostPair.PublicKey, rh) {
		t.Errorf("VerifyCommitment host returned false")
	}

	// 3. Ed25519 Identity Keys & Fingerprints
	clientSeed := decodeHexOrPanic("3333333333333333333333333333333333333333333333333333333333333333")
	hostSeed := decodeHexOrPanic("6666666666666666666666666666666666666666666666666666666666666666")

	clientPriv := ed25519.NewKeyFromSeed(clientSeed)
	hostPriv := ed25519.NewKeyFromSeed(hostSeed)

	clientPub := clientPriv.Public().(ed25519.PublicKey)
	hostPub := hostPriv.Public().(ed25519.PublicKey)

	clientID := sha256.Sum256(clientPub)
	hostID := sha256.Sum256(hostPub)

	expectedClientID := "6c8f8607dbe87077a62a2990ce07d94aaf749df76f87b98eb786a6d10f030765"
	if hex.EncodeToString(clientID[:]) != expectedClientID {
		t.Errorf("ClientID = %s, want %s", hex.EncodeToString(clientID[:]), expectedClientID)
	}

	expectedHostID := "f7b7676c94df7e8fd9998e38f9fff04d8588b00d1801764e3f1877a3feae4477"
	if hex.EncodeToString(hostID[:]) != expectedHostID {
		t.Errorf("HostID = %s, want %s", hex.EncodeToString(hostID[:]), expectedHostID)
	}

	// 4. Nonce & Session Key
	nh := decodeHexOrPanic("7777777777777777777777777777777777777777777777777777777777777777")
	sessionKeyHash := sha256.Sum256([]byte("flitnix-session-token-test-secret-1234"))

	// 5. Binary Transcript
	transcript, err := BuildTranscript(TranscriptParams{
		HostID:     hostID[:],
		ClientID:   clientID[:],
		CommitC:    commitC[:],
		CommitH:    commitH[:],
		Ec:         clientPair.PublicKey,
		Eh:         hostPair.PublicKey,
		Nh:         nh,
		SessionKey: sessionKeyHash[:],
	})
	if err != nil {
		t.Fatalf("BuildTranscript failed: %v", err)
	}
	if len(transcript) != 271 {
		t.Fatalf("transcript length = %d, want 271", len(transcript))
	}

	transcriptHash, err := ComputeTranscriptHash(transcript)
	if err != nil {
		t.Fatalf("ComputeTranscriptHash failed: %v", err)
	}
	expectedTranscriptHash := "221d36b551a597904e8f51d18656abbdd3e99dda46c1e496cfbf734e5d367137"
	if hex.EncodeToString(transcriptHash[:]) != expectedTranscriptHash {
		t.Errorf("transcriptHash = %s, want %s", hex.EncodeToString(transcriptHash[:]), expectedTranscriptHash)
	}

	// 6. HKDF Extraction & Expansion
	prk := DeriveHKDF_PRK(nh, kClient)
	expectedPRK := "c2212bfaa16a4f399ac709a9e3aab13e95e1ce4f77bd3c397e0b7561f4bb8f0c"
	if hex.EncodeToString(prk) != expectedPRK {
		t.Errorf("PRK = %s, want %s", hex.EncodeToString(prk), expectedPRK)
	}

	expandInfo := BuildExpandInfo(transcriptHash)
	if len(expandInfo) != 54 {
		t.Fatalf("expandInfo length = %d, want 54", len(expandInfo))
	}
	expectedExpandInfo := "666c69746e69782d7361732d657870616e642d763100221d36b551a597904e8f51d18656abbdd3e99dda46c1e496cfbf734e5d367137"
	if hex.EncodeToString(expandInfo) != expectedExpandInfo {
		t.Errorf("expandInfo = %s, want %s", hex.EncodeToString(expandInfo), expectedExpandInfo)
	}

	okm := DeriveOKM(prk, expandInfo)
	expectedOKM := "3b26106805379aa8e55574bce8cdf85f7cc2e14ac05baa7aa2ac01ac34f82eab"
	if hex.EncodeToString(okm) != expectedOKM {
		t.Errorf("OKM = %s, want %s", hex.EncodeToString(okm), expectedOKM)
	}

	// 7. Unbiased Rejection Sampling
	num, disp, idx, err := DeriveSAS(okm)
	if err != nil {
		t.Fatalf("DeriveSAS failed: %v", err)
	}
	if idx != 0 {
		t.Errorf("accepted candidate index = %d, want 0", idx)
	}
	if num != 350312 {
		t.Errorf("SASNumeric = %d, want 350312", num)
	}
	if disp != "350-312" {
		t.Errorf("SASDisplay = %q, want '350-312'", disp)
	}

	// 8. Proof Payloads & Signatures
	hostProofPayload := BuildHostProofPayload(transcriptHash)
	if len(hostProofPayload) != 54 {
		t.Errorf("hostProofPayload length = %d, want 54", len(hostProofPayload))
	}
	hostProofSig := SignMessage(hostPriv, hostProofPayload)
	expectedHostSig := "0822116d294daf732ab4ebb805fed4c9f31260644a4e5a0638ef77ede4eb59c91c2b1e91e4aa3b9f834d85f4a01ac06a15e8d837470a8b849685f892a8704e06"
	if hostProofSig != expectedHostSig {
		t.Errorf("hostProofSig = %s, want %s", hostProofSig, expectedHostSig)
	}

	clientProofPayload := BuildClientProofPayload(transcriptHash)
	if len(clientProofPayload) != 56 {
		t.Errorf("clientProofPayload length = %d, want 56", len(clientProofPayload))
	}
	clientProofSig := SignClientProof(clientPriv, transcriptHash)
	expectedClientSig := "6488c750a9889acb52037a78532af644ca3807ec7d0035d824717505fd8b10304b694fcd419907e7fd14ee4f41d987566df0a6136db902e051611514759b7707"
	if clientProofSig != expectedClientSig {
		t.Errorf("clientProofSig = %s, want %s", clientProofSig, expectedClientSig)
	}

	// Verify proofs
	validClientProof, err := VerifyClientProof(clientPub, transcriptHash, clientProofSig)
	if err != nil || !validClientProof {
		t.Fatalf("VerifyClientProof failed: %v", err)
	}
}

func TestTestVectorBSyntheticHKDF(t *testing.T) {
	// Transcript hash = SHA-256("abc")
	inputHash := decodeHexOrPanic("ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad")
	var tHash [32]byte
	copy(tHash[:], inputHash)

	prk := decodeHexOrPanic("e7e163a5d5beea49bddeefde311775b9b90edb9a79e18ad0eba8b9c1eb8ae1c3")
	expandInfo := BuildExpandInfo(tHash)

	expectedExpandInfo := "666c69746e69782d7361732d657870616e642d763100ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if hex.EncodeToString(expandInfo) != expectedExpandInfo {
		t.Errorf("expandInfo = %s, want %s", hex.EncodeToString(expandInfo), expectedExpandInfo)
	}

	okm := DeriveOKM(prk, expandInfo)
	expectedOKM := "6761d6296a5dcf0777c257a4afedec58767a974b7a9896718d18024462d07f7b"
	if hex.EncodeToString(okm) != expectedOKM {
		t.Errorf("OKM = %s, want %s", hex.EncodeToString(okm), expectedOKM)
	}

	num, disp, idx, err := DeriveSAS(okm)
	if err != nil {
		t.Fatalf("DeriveSAS failed: %v", err)
	}
	if idx != 0 {
		t.Errorf("idx = %d, want 0", idx)
	}
	if num != 465065 {
		t.Errorf("numeric = %d, want 465065", num)
	}
	if disp != "465-065" {
		t.Errorf("display = %q, want '465-065'", disp)
	}
}

func TestSASRejectionSamplingFailClosed(t *testing.T) {
	// Construct OKM where all eight 32-bit candidates are >= 4,294,000,000 (0xFFF0BDC0)
	// 0xFFFFFFFF = 4,294,967,295 >= 4,294,000,000
	badOKM := make([]byte, 32)
	for i := range badOKM {
		badOKM[i] = 0xFF
	}

	_, _, _, err := DeriveSAS(badOKM)
	if err != ErrSASDerivationExhausted {
		t.Fatalf("got err=%v, want ErrSASDerivationExhausted", err)
	}
}

func TestHostIdentityLifecycle(t *testing.T) {
	tmpDir := t.TempDir()
	idPath := filepath.Join(tmpDir, "host_identity.json")

	h1, err := LoadOrCreateHostIdentity(idPath)
	if err != nil {
		t.Fatalf("LoadOrCreateHostIdentity failed: %v", err)
	}
	if h1.HostID == "" || h1.PublicKey == "" || h1.PrivateKey == "" {
		t.Fatalf("HostIdentity missing fields: %+v", h1)
	}

	// Reload from disk
	h2, err := LoadOrCreateHostIdentity(idPath)
	if err != nil {
		t.Fatalf("Reload HostIdentity failed: %v", err)
	}
	if h2.HostID != h1.HostID || h2.PublicKey != h1.PublicKey {
		t.Fatalf("Reload mismatch: %+v vs %+v", h2, h1)
	}

	// Test Proof signing and verification
	tHash := sha256.Sum256([]byte("test transcript"))

	sig := h2.SignHostProof(tHash)
	valid, err := h2.VerifyHostProof(tHash, sig)
	if err != nil || !valid {
		t.Fatalf("VerifyHostProof failed: valid=%v, err=%v", valid, err)
	}
}
