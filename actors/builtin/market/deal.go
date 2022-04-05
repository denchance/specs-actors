package market

import (
	"bytes"
	"fmt"
	"io"
	"unicode/utf8"

	addr "github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/big"
	acrypto "github.com/filecoin-project/go-state-types/crypto"
	market0 "github.com/filecoin-project/specs-actors/actors/builtin/market"
	"github.com/ipfs/go-cid"
	cbg "github.com/whyrusleeping/cbor-gen"
	"golang.org/x/xerrors"
)

//var PieceCIDPrefix = cid.Prefix{
//	Version:  1,
//	Codec:    cid.FilCommitmentUnsealed,
//	MhType:   mh.SHA2_256_TRUNC254_PADDED,
//	MhLength: 32,
//}
var PieceCIDPrefix = market0.PieceCIDPrefix

// The DealLabel is a kinded union of string or byte slice.
// It serializes to a CBOR string or CBOR byte string depending on which form it takes.
// The empty label is serialized as an empty CBOR string (maj type 3).
type DealLabel struct {
	s  *string
	bs *[]byte
}

// Zero value of DealLabel is canonical EmptyDealLabel but three different values serialize the same way:
// DealLabel{nil, nil}, (*DealLabel(nil), and DealLabel{&"", nil})
var EmptyDealLabel = DealLabel{s: nil, bs: nil}

func NewLabelFromString(s string) (DealLabel, error) {
	if len(s) > DealMaxLabelSize {
		return EmptyDealLabel, xerrors.Errorf("provided string is too large to be a label (%d), max length (%d)", len(s), DealMaxLabelSize)
	}
	if !utf8.ValidString(s) {
		return EmptyDealLabel, xerrors.Errorf("provided string is invalid utf8")
	}
	return DealLabel{
		s: &s,
	}, nil
}

func NewLabelFromBytes(b []byte) (DealLabel, error) {
	if len(b) > DealMaxLabelSize {
		return EmptyDealLabel, xerrors.Errorf("provided bytes are too large to be a label (%d), max length (%d)", len(b), DealMaxLabelSize)
	}

	return DealLabel{
		bs: &b,
	}, nil
}

func (label DealLabel) IsEmpty() bool {
	return label.s == nil && label.bs == nil
}

func (label DealLabel) IsString() bool {
	return label.s != nil
}

func (label DealLabel) IsBytes() bool {
	return label.bs != nil
}

func (label DealLabel) ToString() (string, error) {
	if !label.IsString() {
		return "", xerrors.Errorf("label is not string")
	}

	return *label.s, nil
}

func (label DealLabel) ToBytes() []byte {
	if label.IsBytes() {
		return *label.bs
	}
	if label.IsString() {
		return []byte(*label.s)

	}
	// empty label, bytes are those of empty string
	return []byte("")
}

func (label DealLabel) Length() int {
	if label.IsString() {
		return len(*label.s)
	}
	if label.IsBytes() {
		return len(*label.bs)
	}
	// empty
	return 0

	return len(*label.bs)
}
func (l DealLabel) Equals(o DealLabel) bool {
	if l.IsString() && o.IsString() {
		return *l.s == *o.s
	} else if l.IsBytes() && o.IsBytes() {
		return bytes.Equal(*l.bs, *o.bs)
	} else if l.IsEmpty() && o.IsEmpty() {
		return true
	} else {
		return false
	}
}

func (label *DealLabel) MarshalCBOR(w io.Writer) error {
	scratch := make([]byte, 9)

	// nil *DealLabel counts as EmptyLabel
	// on chain structures should never have a pointer to a DealLabel but the case is included for completeness
	if label == nil || label.IsEmpty() {
		if err := cbg.WriteMajorTypeHeaderBuf(scratch, w, cbg.MajTextString, 0); err != nil {
			return err
		}
		if _, err := io.WriteString(w, string("")); err != nil {
			return err
		}
		return nil
	} else if label.IsString() && label.IsBytes() {
		return fmt.Errorf("dealLabel cannot have both string and bytes set")
	} else if label.IsBytes() {
		if len(*label.bs) > cbg.ByteArrayMaxLen {
			return xerrors.Errorf("labelBytes is too long to marshal (%d), max allowed (%d)", len(*label.bs), cbg.ByteArrayMaxLen)
		}

		if err := cbg.WriteMajorTypeHeaderBuf(scratch, w, cbg.MajByteString, uint64(len(*label.bs))); err != nil {
			return err
		}

		if _, err := w.Write((*label.bs)[:]); err != nil {
			return err
		}
	} else { // label.IsString()
		if err := cbg.WriteMajorTypeHeaderBuf(scratch, w, cbg.MajTextString, uint64(len(*label.s))); err != nil {
			return err
		}
		if _, err := io.WriteString(w, *label.s); err != nil {
			return err
		}
	}

	return nil
}

func (label *DealLabel) UnmarshalCBOR(br io.Reader) error {
	if label == nil {
		return xerrors.Errorf("cannot unmarshal into nil pointer")
	}

	// reset fields
	label.s = nil
	label.bs = nil

	scratch := make([]byte, 8)

	maj, length, err := cbg.CborReadHeaderBuf(br, scratch)
	if err != nil {
		return err
	}

	if maj == cbg.MajTextString {

		if length > cbg.MaxLength {
			return fmt.Errorf("label string was too long (%d), max allowed (%d)", length, cbg.MaxLength)
		}

		buf := make([]byte, length)
		_, err = io.ReadAtLeast(br, buf, int(length))
		if err != nil {
			return err
		}
		s := string(buf)
		if !utf8.ValidString(s) {
			return fmt.Errorf("label string not valid utf8")
		}
		label.s = &s
	} else if maj == cbg.MajByteString {

		if length > cbg.ByteArrayMaxLen {
			return fmt.Errorf("label bytes was too long (%d), max allowed (%d)", length, cbg.ByteArrayMaxLen)
		}

		bs := make([]uint8, length)
		label.bs = &bs

		if _, err := io.ReadFull(br, bs[:]); err != nil {
			return err
		}
	} else {
		return fmt.Errorf("unexpected major tag (%d) when unmarshaling DealLabel: only textString (%d) or byteString (%d) expected", maj, cbg.MajTextString, cbg.MajByteString)
	}

	return nil
}

// Note: Deal Collateral is only released and returned to clients and miners
// when the storage deal stops counting towards power. In the current iteration,
// it will be released when the sector containing the storage deals expires,
// even though some storage deals can expire earlier than the sector does.
// Collaterals are denominated in PerEpoch to incur a cost for self dealing or
// minimal deals that last for a long time.
// Note: ClientCollateralPerEpoch may not be needed and removed pending future confirmation.
// There will be a Minimum value for both client and provider deal collateral.
type DealProposal struct {
	PieceCID     cid.Cid `checked:"true"` // Checked in validateDeal, CommP
	PieceSize    abi.PaddedPieceSize
	VerifiedDeal bool
	Client       addr.Address
	Provider     addr.Address

	// Label is an arbitrary client chosen label to apply to the deal
	Label DealLabel

	// Nominal start epoch. Deal payment is linear between StartEpoch and EndEpoch,
	// with total amount StoragePricePerEpoch * (EndEpoch - StartEpoch).
	// Storage deal must appear in a sealed (proven) sector no later than StartEpoch,
	// otherwise it is invalid.
	StartEpoch           abi.ChainEpoch
	EndEpoch             abi.ChainEpoch
	StoragePricePerEpoch abi.TokenAmount

	ProviderCollateral abi.TokenAmount
	ClientCollateral   abi.TokenAmount
}

// ClientDealProposal is a DealProposal signed by a client
type ClientDealProposal struct {
	Proposal        DealProposal
	ClientSignature acrypto.Signature
}

func (p *DealProposal) Duration() abi.ChainEpoch {
	return p.EndEpoch - p.StartEpoch
}

func (p *DealProposal) TotalStorageFee() abi.TokenAmount {
	return big.Mul(p.StoragePricePerEpoch, big.NewInt(int64(p.Duration())))
}

func (p *DealProposal) ClientBalanceRequirement() abi.TokenAmount {
	return big.Add(p.ClientCollateral, p.TotalStorageFee())
}

func (p *DealProposal) ProviderBalanceRequirement() abi.TokenAmount {
	return p.ProviderCollateral
}

func (p *DealProposal) Cid() (cid.Cid, error) {
	buf := new(bytes.Buffer)
	if err := p.MarshalCBOR(buf); err != nil {
		return cid.Undef, err
	}
	return abi.CidBuilder.Sum(buf.Bytes())
}
