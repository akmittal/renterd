package stores

import (
	"context"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	"github.com/klauspost/reedsolomon"
	"go.sia.tech/core/types"
	"go.sia.tech/renterd/api"
	"go.sia.tech/renterd/object"
	"lukechampine.com/frand"
)

func TestMultipartUploadWithUploadPackingRegression(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	dir := t.TempDir()
	db, _, _, err := newTestSQLStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	// create 30 hosts
	hks, err := db.addTestHosts(30)
	if err != nil {
		t.Fatal(err)
	}

	// create one contract for each host.
	fcids, _, err := db.addTestContracts(hks)
	if err != nil {
		t.Fatal(err)
	}

	usedContracts := make(map[types.PublicKey]types.FileContractID)
	for i := range hks {
		usedContracts[hks[i]] = fcids[i]
	}

	ctx := context.Background()
	minShards := uint8(10)
	totalShards := uint8(30)
	objName := "/foo"
	partSize := 8 * 1 << 20
	nParts := 6
	totalSize := int64(nParts * partSize)

	// Upload parts until we have enough data for 2 buffers.
	resp, err := db.CreateMultipartUpload(ctx, api.DefaultBucketName, objName, object.NoOpKey, testMimeType)
	if err != nil {
		t.Fatal(err)
	}
	var parts []api.MultipartCompletedPart
	for i := 1; i <= nParts; i++ {
		partialSlabs, _, err := db.AddPartialSlab(ctx, frand.Bytes(partSize), minShards, totalShards, testContractSet)
		if err != nil {
			t.Fatal(err)
		}
		etag := hex.EncodeToString(frand.Bytes(16))
		err = db.AddMultipartPart(ctx, api.DefaultBucketName, objName, testContractSet, etag, resp.UploadID, i, []object.SlabSlice{}, partialSlabs, usedContracts)
		if err != nil {
			t.Fatal(err)
		}
		parts = append(parts, api.MultipartCompletedPart{
			PartNumber: i,
			ETag:       etag,
		})
	}
	_, err = db.CompleteMultipartUpload(ctx, api.DefaultBucketName, objName, resp.UploadID, parts)
	if err != nil {
		t.Fatal(err)
	}

	// Fetch the object.
	obj, err := db.Object(ctx, api.DefaultBucketName, objName)
	if err != nil {
		t.Fatal(err)
	} else if obj.Size != int64(totalSize) {
		t.Fatalf("expected object size to be %v, got %v", totalSize, obj.Size)
	} else if obj.TotalSize() != totalSize {
		t.Fatalf("expected object total size to be %v, got %v", totalSize, obj.TotalSize())
	}

	// Upload buffers.
	upload := func(ps api.PackedSlab) api.UploadedPackedSlab {
		ups := api.UploadedPackedSlab{
			BufferID: ps.BufferID,
		}
		rs, _ := reedsolomon.New(int(minShards), int(totalShards-minShards))
		splitData, err := rs.Split(ps.Data)
		if err != nil {
			t.Fatal(err)
		}
		err = rs.Encode(splitData)
		if err != nil {
			t.Fatal(err)
		}
		for i, shard := range splitData {
			ups.Shards = append(ups.Shards, object.Sector{
				Host: hks[i],
				Root: types.HashBytes(shard),
			})
		}
		return ups
	}
	packedSlabs, err := db.PackedSlabsForUpload(ctx, time.Hour, minShards, totalShards, testContractSet, 2)
	if err != nil {
		t.Fatal(err)
	}
	var uploadedPackedSlabs []api.UploadedPackedSlab
	for _, ps := range packedSlabs {
		uploadedPackedSlabs = append(uploadedPackedSlabs, upload(ps))
	}
	if err := db.MarkPackedSlabsUploaded(ctx, uploadedPackedSlabs, usedContracts); err != nil {
		t.Fatal(err)
	}

	// Fetch the object again.
	obj, err = db.Object(ctx, api.DefaultBucketName, objName)
	if err != nil {
		t.Fatal(err)
	} else if obj.Size != int64(totalSize) {
		t.Fatalf("expected object size to be %v, got %v", totalSize, obj.Size)
	} else if obj.TotalSize() != totalSize {
		for _, f := range obj.Slabs {
			fmt.Println("slice", f.Length)
		}
		for _, f := range obj.PartialSlabs {
			fmt.Println("ps", f.Length)
		}
		t.Fatalf("expected object total size to be %v, got %v", totalSize, obj.TotalSize())
	}
}
