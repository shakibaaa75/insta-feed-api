package store

import (
	"bytes"
	"context"
	"errors"
	"io"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo/gridfs"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// ErrNoImage is returned when no stored image exists for a shortcode.
var ErrNoImage = errors.New("image not found")

// ------------------------------------------------------------
// HasImage
//
// Reports whether we already hold a copy of this post's image.
// ------------------------------------------------------------

func (s *Store) HasImage(
	ctx context.Context,
	shortcode string,
) bool {

	cur, err := s.bucket.Find(
		bson.M{"filename": shortcode},
		options.GridFSFind().SetLimit(1),
	)

	if err != nil {
		return false
	}

	defer cur.Close(ctx)

	return cur.Next(ctx)
}

// ------------------------------------------------------------
// SaveImage
//
// Stores the image bytes in MongoDB (GridFS) under the shortcode.
// The new copy is uploaded first, then older copies are removed,
// so the image is never missing while it is being replaced.
// ------------------------------------------------------------

func (s *Store) SaveImage(
	ctx context.Context,
	shortcode string,
	data []byte,
) error {

	// Find existing copies.
	var oldIDs []primitive.ObjectID

	cur, err := s.bucket.Find(
		bson.M{"filename": shortcode},
	)

	if err != nil {
		return err
	}

	for cur.Next(ctx) {

		var f struct {
			ID primitive.ObjectID `bson:"_id"`
		}

		if err := cur.Decode(&f); err == nil {
			oldIDs = append(oldIDs, f.ID)
		}
	}

	_ = cur.Close(ctx)

	// Upload the new copy.
	if _, err := s.bucket.UploadFromStream(
		shortcode,
		bytes.NewReader(data),
	); err != nil {
		return err
	}

	// Remove old copies.
	for _, id := range oldIDs {
		_ = s.bucket.Delete(id)
	}

	return nil
}

// ------------------------------------------------------------
// ReadImage
//
// Returns the stored image bytes, or ErrNoImage.
// ------------------------------------------------------------

func (s *Store) ReadImage(
	ctx context.Context,
	shortcode string,
) ([]byte, error) {

	stream, err := s.bucket.OpenDownloadStreamByName(
		shortcode,
	)

	if err != nil {

		if errors.Is(err, gridfs.ErrFileNotFound) {
			return nil, ErrNoImage
		}

		return nil, err
	}

	defer stream.Close()

	return io.ReadAll(stream)
}

// ------------------------------------------------------------
// SetImageURL
//
// Points a post's imageUrl at our own stored copy.
// ------------------------------------------------------------

func (s *Store) SetImageURL(
	ctx context.Context,
	shortcode string,
	imageURL string,
) error {

	_, err := s.posts.UpdateOne(
		ctx,
		bson.M{"shortcode": shortcode},
		bson.M{
			"$set": bson.M{
				"imageUrl": imageURL,
			},
		},
	)

	return err
}
