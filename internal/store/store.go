package store

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/gridfs"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// ------------------------------------------------------------
// Post
// ------------------------------------------------------------

type Post struct {
	Shortcode string    `bson:"shortcode" json:"shortcode"`
	Profile   string    `bson:"profile" json:"profile"`
	URL       string    `bson:"url" json:"url"`
	ImageURL  string    `bson:"imageUrl" json:"imageUrl"`
	PostedAt  time.Time `bson:"postedAt" json:"postedAt"`
	CreatedAt time.Time `bson:"createdAt" json:"createdAt"`
}

// ------------------------------------------------------------
// Status
// ------------------------------------------------------------

type Status struct {
	Profile       string    `bson:"profile" json:"profile"`
	LastRunAt     time.Time `bson:"lastRunAt" json:"lastRunAt"`
	LastSuccessAt time.Time `bson:"lastSuccessAt" json:"lastSuccessAt"`
	LastOK        bool      `bson:"lastOk" json:"lastOk"`
	LastError     string    `bson:"lastError" json:"lastError"`
	LastNew       int       `bson:"lastNew" json:"lastNew"`
}

// ------------------------------------------------------------
// Store
// ------------------------------------------------------------

type Store struct {
	client *mongo.Client
	posts  *mongo.Collection
	status *mongo.Collection
	bucket *gridfs.Bucket // stored copies of the post images
}

// ------------------------------------------------------------
// New
// ------------------------------------------------------------

func New(
	ctx context.Context,
	uri string,
	dbName string,
) (*Store, error) {

	client, err := mongo.Connect(
		ctx,
		options.Client().ApplyURI(uri),
	)

	if err != nil {
		return nil, err
	}

	if err := client.Ping(
		ctx,
		nil,
	); err != nil {
		return nil, err
	}

	db := client.Database(dbName)

	bucket, err := gridfs.NewBucket(
		db,
		options.GridFSBucket().SetName("images"),
	)

	if err != nil {
		return nil, err
	}

	s := &Store{
		client: client,
		posts:  db.Collection("posts"),
		status: db.Collection("status"),
		bucket: bucket,
	}

	// Unique index:
	// the same Instagram post can never be stored twice.
	_, err = s.posts.Indexes().CreateOne(
		ctx,
		mongo.IndexModel{
			Keys: bson.D{
				{
					Key:   "shortcode",
					Value: 1,
				},
			},
			Options: options.Index().SetUnique(true),
		},
	)

	if err != nil {
		return nil, err
	}

	return s, nil
}

// ------------------------------------------------------------
// Close
// ------------------------------------------------------------

func (s *Store) Close(
	ctx context.Context,
) {
	_ = s.client.Disconnect(ctx)
}

// ------------------------------------------------------------
// AddPosts
//
// Inserts new posts.
//
// If a post already exists and the extension now provides an
// image URL, we update the existing MongoDB document with it.
//
// This is important because your existing 72 posts can gradually
// receive imageUrl when the extension sees them again.
// ------------------------------------------------------------

func (s *Store) AddPosts(
	ctx context.Context,
	posts []Post,
) (int, error) {

	added := 0
	now := time.Now().UTC()

	for _, p := range posts {

		setOnInsert := bson.M{
			"profile":   p.Profile,
			"url":       p.URL,
			"createdAt": now,
		}

		update := bson.M{}

		// If this is a real Instagram date,
		// update the stored postedAt value.
		if p.PostedAt.Year() > 2010 {
			update["$set"] = bson.M{
				"postedAt": p.PostedAt,
			}
		} else {
			setOnInsert["postedAt"] = p.PostedAt
		}

		// If we have an image URL, update the document.
		//
		// This means old posts that already exist in MongoDB
		// can acquire an imageUrl the next time the extension
		// encounters them.
		if p.ImageURL != "" {
			if existingSet, ok := update["$set"].(bson.M); ok {
				existingSet["imageUrl"] = p.ImageURL
			} else {
				update["$set"] = bson.M{
					"imageUrl": p.ImageURL,
				}
			}
		}

		update["$setOnInsert"] = setOnInsert

		res, err := s.posts.UpdateOne(
			ctx,
			bson.M{
				"shortcode": p.Shortcode,
			},
			update,
			options.Update().SetUpsert(true),
		)

		if err != nil {
			return added, err
		}

		if res.UpsertedCount > 0 {
			added++
		}
	}

	return added, nil
}

// ------------------------------------------------------------
// AddNew
//
// Old links-only importer.
// Existing posts are not modified here.
// ------------------------------------------------------------

func (s *Store) AddNew(
	ctx context.Context,
	posts []Post,
) (int, error) {

	added := 0
	now := time.Now().UTC()

	for _, p := range posts {

		setOnInsert := bson.M{
			"profile":   p.Profile,
			"url":       p.URL,
			"postedAt":  p.PostedAt,
			"createdAt": now,
		}

		if p.ImageURL != "" {
			setOnInsert["imageUrl"] = p.ImageURL
		}

		res, err := s.posts.UpdateOne(
			ctx,
			bson.M{
				"shortcode": p.Shortcode,
			},
			bson.M{
				"$setOnInsert": setOnInsert,
			},
			options.Update().SetUpsert(true),
		)

		if err != nil {
			return added, err
		}

		if res.UpsertedCount > 0 {
			added++
		}
	}

	return added, nil
}

// ------------------------------------------------------------
// List
//
// Returns posts newest first.
// An empty profile returns all profiles.
// ------------------------------------------------------------

func (s *Store) List(
	ctx context.Context,
	profile string,
	limit int64,
) ([]Post, error) {

	filter := bson.M{}

	if profile != "" {
		filter["profile"] = profile
	}

	cur, err := s.posts.Find(
		ctx,
		filter,
		options.Find().
			SetSort(
				bson.D{
					{
						Key:   "postedAt",
						Value: -1,
					},
				},
			).
			SetLimit(limit),
	)

	if err != nil {
		return nil, err
	}

	out := []Post{}

	if err := cur.All(
		ctx,
		&out,
	); err != nil {
		return nil, err
	}

	return out, nil
}

// ------------------------------------------------------------
// SaveStatus
// ------------------------------------------------------------

func (s *Store) SaveStatus(
	ctx context.Context,
	profile string,
	added int,
	runErr error,
) error {

	now := time.Now().UTC()

	set := bson.M{
		"lastRunAt": now,
		"lastOk":    runErr == nil,
		"lastNew":   added,
		"lastError": "",
	}

	if runErr != nil {
		set["lastError"] = runErr.Error()
	} else {
		set["lastSuccessAt"] = now
	}

	_, err := s.status.UpdateOne(
		ctx,
		bson.M{
			"profile": profile,
		},
		bson.M{
			"$set": set,
		},
		options.Update().SetUpsert(true),
	)

	return err
}

// ------------------------------------------------------------
// Statuses
// ------------------------------------------------------------

func (s *Store) Statuses(
	ctx context.Context,
) ([]Status, error) {

	cur, err := s.status.Find(
		ctx,
		bson.M{},
	)

	if err != nil {
		return nil, err
	}

	out := []Status{}

	if err := cur.All(
		ctx,
		&out,
	); err != nil {
		return nil, err
	}

	return out, nil
}
