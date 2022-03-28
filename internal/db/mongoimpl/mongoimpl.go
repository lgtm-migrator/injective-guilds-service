package mongoimpl

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/InjectiveLabs/injective-guilds-service/internal/db"
	"github.com/InjectiveLabs/injective-guilds-service/internal/db/model"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const (
	connectionTimeout              = 30 * time.Second
	GuildCollectionName            = "guilds"
	MemberCollectionName           = "members"
	AccountPortfolioCollectionName = "account_portfolios"
	GuildPortfolioCollectionName   = "guild_portfolios"
	DenomCollectionName            = "denoms"
)

var (
	ErrNotFound        = errors.New("dberr: not found")
	ErrMemberExceedCap = errors.New("member exceeds cap")
	ErrAlreadyMember   = errors.New("already member")
)

type MongoImpl struct {
	db.DBService

	client  *mongo.Client
	session mongo.Session

	guildCollection            *mongo.Collection
	memberCollection           *mongo.Collection
	accountPortfolioCollection *mongo.Collection
	guildPortfolioCollection   *mongo.Collection
	denomCollection            *mongo.Collection
}

func NewService(ctx context.Context, connectionURL, databaseName string) (db.DBService, error) {
	ctx, cancel := context.WithTimeout(ctx, connectionTimeout)
	defer cancel()

	client, err := mongo.Connect(ctx, options.Client().ApplyURI(connectionURL))
	if err != nil {
		return nil, fmt.Errorf("connect mongo err: %w", err)
	}

	session, err := client.StartSession()
	if err != nil {
		return nil, fmt.Errorf("new session err: %w", err)
	}

	return &MongoImpl{
		client:                     client,
		session:                    session,
		guildCollection:            client.Database(databaseName).Collection(GuildCollectionName),
		memberCollection:           client.Database(databaseName).Collection(MemberCollectionName),
		accountPortfolioCollection: client.Database(databaseName).Collection(AccountPortfolioCollectionName),
		guildPortfolioCollection:   client.Database(databaseName).Collection(GuildPortfolioCollectionName),
		denomCollection:            client.Database(databaseName).Collection(DenomCollectionName),
	}, nil
}

func makeIndex(unique bool, keys interface{}) mongo.IndexModel {
	idx := mongo.IndexModel{
		Keys:    keys,
		Options: options.Index().SetUnique(unique),
	}
	return idx
}

func (s *MongoImpl) EnsureIndex(ctx context.Context) error {
	// use CreateMany here for future custom
	_, err := s.memberCollection.Indexes().CreateMany(ctx, []mongo.IndexModel{
		makeIndex(true, bson.D{{Key: "injective_address", Value: 1}}),
		makeIndex(false, bson.D{{Key: "is_default_guild_member", Value: 1}}),
		makeIndex(false, bson.D{{Key: "guild_id", Value: 1}}),
	})
	if err != nil {
		return err
	}

	_, err = s.accountPortfolioCollection.Indexes().CreateMany(ctx, []mongo.IndexModel{
		makeIndex(false, bson.D{{Key: "injective_address", Value: 1}}),
		makeIndex(false, bson.D{{Key: "guild_id", Value: 1}}),
		makeIndex(false, bson.D{{Key: "updated_at", Value: -1}}),
	})
	if err != nil {
		return err
	}

	_, err = s.guildPortfolioCollection.Indexes().CreateMany(ctx, []mongo.IndexModel{
		makeIndex(false, bson.D{{Key: "guild_id", Value: 1}}),
		makeIndex(false, bson.D{{Key: "updated_at", Value: -1}}),
	})
	if err != nil {
		return err
	}

	return nil
}

func (s *MongoImpl) ListGuildPortfolios(
	ctx context.Context,
	filter model.GuildPortfoliosFilter,
) (result []*model.GuildPortfolio, err error) {
	guildObjectID, err := primitive.ObjectIDFromHex(filter.GuildID)
	if err != nil {
		return nil, fmt.Errorf("cannot parse guildID: %w", err)
	}

	portfolioFilter := bson.M{
		"guild_id": guildObjectID,
	}

	var updatedAtFilter = make(bson.M)
	if filter.StartTime != nil {
		updatedAtFilter["$gte"] = *filter.StartTime
	}

	if filter.EndTime != nil {
		updatedAtFilter["$lt"] = *filter.EndTime
	}

	if len(updatedAtFilter) > 0 {
		portfolioFilter["updated_at"] = updatedAtFilter
	}

	opt := &options.FindOptions{}
	opt.SetSort(bson.M{"updated_at": -1})
	if filter.Limit != nil {
		opt.SetLimit(*filter.Limit)
	}

	cur, err := s.guildPortfolioCollection.Find(ctx, portfolioFilter, opt)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	for cur.Next(ctx) {
		var guildPortfolio model.GuildPortfolio
		err := cur.Decode(&guildPortfolio)
		if err != nil {
			return nil, err
		}

		result = append(result, &guildPortfolio)
	}

	return result, nil
}

func (s *MongoImpl) AddGuild(ctx context.Context, guild *model.Guild) (*primitive.ObjectID, error) {
	insertOneRes, err := s.guildCollection.InsertOne(ctx, guild)
	if err != nil {
		return nil, err
	}

	objID := insertOneRes.InsertedID.(primitive.ObjectID)
	return &objID, nil
}

func (s *MongoImpl) DeleteGuild(ctx context.Context, guildID string) error {
	guildObjectID, err := primitive.ObjectIDFromHex(guildID)
	if err != nil {
		return fmt.Errorf("cannot parse guildID: %w", err)
	}

	_, err = s.session.WithTransaction(ctx, func(sessCtx mongo.SessionContext) (interface{}, error) {
		filter := bson.M{
			"_id": guildObjectID,
		}
		_, err := s.guildCollection.DeleteOne(ctx, filter)
		if err != nil {
			return nil, err
		}

		filter = bson.M{
			"guild_id": guildObjectID,
		}

		_, err = s.memberCollection.DeleteMany(ctx, filter)
		if err != nil {
			return nil, err
		}

		_, err = s.accountPortfolioCollection.DeleteMany(ctx, filter)
		if err != nil {
			return nil, err
		}

		_, err = s.guildPortfolioCollection.DeleteMany(ctx, filter)
		if err != nil {
			return nil, err
		}

		return nil, nil
	})
	return err
}

func (s *MongoImpl) ListAllGuilds(ctx context.Context) (result []*model.Guild, err error) {
	filter := bson.M{}
	cur, err := s.guildCollection.Find(ctx, filter)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	for cur.Next(ctx) {
		var guild model.Guild
		err := cur.Decode(&guild)
		if err != nil {
			return nil, err
		}

		result = append(result, &guild)
	}

	return result, nil
}

func (s *MongoImpl) GetSingleGuild(ctx context.Context, guildID string) (*model.Guild, error) {
	guildObjectID, err := primitive.ObjectIDFromHex(guildID)
	if err != nil {
		return nil, fmt.Errorf("cannot parse guildID: %w", err)
	}

	filter := bson.M{
		"_id": guildObjectID,
	}

	res := s.guildCollection.FindOne(ctx, filter)
	if err := res.Err(); err != nil {
		return nil, err
	}

	var guild model.Guild
	if err := res.Decode(&guild); err != nil {
		return nil, err
	}

	return &guild, nil
}

func (s *MongoImpl) AddGuildPortfolios(ctx context.Context, portfolios []*model.GuildPortfolio) error {
	docs := make([]interface{}, len(portfolios))
	for i, p := range portfolios {
		docs[i] = p
	}

	_, err := s.guildPortfolioCollection.InsertMany(ctx, docs)
	return err
}

func (s *MongoImpl) ListGuildMembers(
	ctx context.Context,
	memberFilter model.MemberFilter,
) (result []*model.GuildMember, err error) {
	filter := bson.M{}

	if memberFilter.GuildID != nil {
		guildObjectID, err := primitive.ObjectIDFromHex(*memberFilter.GuildID)
		if err != nil {
			return nil, fmt.Errorf("cannot parse guildID: %w", err)
		}
		filter["guild_id"] = guildObjectID
	}

	if memberFilter.IsDefaultMember != nil {
		filter["is_default_guild_member"] = *memberFilter.IsDefaultMember
	}

	if memberFilter.InjectiveAddress != nil {
		filter["injective_address"] = *memberFilter.InjectiveAddress
	}

	cur, err := s.memberCollection.Find(ctx, filter)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	for cur.Next(ctx) {
		var member model.GuildMember
		err := cur.Decode(&member)
		if err != nil {
			return nil, err
		}

		result = append(result, &member)
	}

	return result, nil
}

func (s *MongoImpl) upsertMember(
	ctx context.Context,
	guildID primitive.ObjectID,
	address model.Address,
	isDefaultMember bool,
) (*mongo.UpdateResult, error) {
	filter := bson.M{
		"injective_address": address.String(),
	}
	upd := bson.M{
		"$set": bson.M{
			"guild_id":                guildID,
			"is_default_guild_member": isDefaultMember,
			"since":                   time.Now(),
		},
	}
	updOpt := &options.UpdateOptions{}
	updOpt.SetUpsert(true)

	return s.memberCollection.UpdateOne(ctx, filter, upd, updOpt)
}

func (s *MongoImpl) deleteMember(
	ctx context.Context,
	guildID primitive.ObjectID,
	address model.Address,
) (*mongo.DeleteResult, error) {
	filter := bson.M{
		"guild_id":          guildID,
		"injective_address": address.String(),
	}

	return s.memberCollection.DeleteOne(ctx, filter)
}

// do we want to keep data for future analyze?
func (s *MongoImpl) deletePortfolios(
	ctx context.Context,
	guildID primitive.ObjectID,
	address model.Address,
) (*mongo.DeleteResult, error) {
	filter := bson.M{
		"guild_id":          guildID,
		"injective_address": address.String(),
	}

	return s.memberCollection.DeleteMany(ctx, filter)
}

func (s *MongoImpl) adjustMemberCount(
	ctx context.Context,
	guildID primitive.ObjectID,
	increment int,
) (*mongo.UpdateResult, error) {
	filter := bson.M{
		"_id": guildID,
	}
	upd := bson.M{
		"$inc": bson.M{
			"member_count": increment,
		},
	}
	return s.guildCollection.UpdateOne(ctx, filter, upd)
}

func (s *MongoImpl) AddMember(ctx context.Context, guildID string, address model.Address, isDefaultMember bool) error {
	guildObjectID, err := primitive.ObjectIDFromHex(guildID)
	if err != nil {
		return fmt.Errorf("cannot parse guildID: %w", err)
	}

	_, err = s.session.WithTransaction(ctx, func(sessCtx mongo.SessionContext) (interface{}, error) {
		// add default member don't require any check
		// just insert (check for unique only)
		if !isDefaultMember {
			guild, err := s.GetSingleGuild(sessCtx, guildID)
			if err != nil {
				return nil, err
			}

			if guild.MemberCount >= guild.Capacity {
				return nil, ErrMemberExceedCap
			}

			_, err = s.adjustMemberCount(sessCtx, guildObjectID, 1)
			if err != nil {
				return nil, err
			}
		}

		upsertRes, err := s.upsertMember(sessCtx, guildObjectID, address, isDefaultMember)
		if err != nil {
			return nil, err
		}

		// duplicate member, revert transaction
		if upsertRes.UpsertedCount < 1 {
			return nil, ErrAlreadyMember
		}

		return nil, nil
	})

	return err
}

func (s *MongoImpl) RemoveMember(ctx context.Context, guildID string, address model.Address) error {
	guildObjectID, err := primitive.ObjectIDFromHex(guildID)
	if err != nil {
		return fmt.Errorf("cannot parse guildID: %w", err)
	}

	_, err = s.session.WithTransaction(ctx, func(sessCtx mongo.SessionContext) (interface{}, error) {
		deleteRes, err := s.deleteMember(ctx, guildObjectID, address)
		if err != nil {
			return nil, err
		}

		// expected to have 1 account deleted
		if deleteRes.DeletedCount != 1 {
			return nil, errors.New("cannot delete")
		}

		_, err = s.adjustMemberCount(sessCtx, guildObjectID, -1)
		if err != nil {
			return nil, err
		}

		_, err = s.deletePortfolios(ctx, guildObjectID, address)
		if err != nil {
			return nil, err
		}

		return nil, nil
	})

	return err
}

// account portfolio gets latest account portfolio
// TODO: Unify getAccountPortfolio to 1 function
func (s *MongoImpl) GetAccountPortfolio(ctx context.Context, address model.Address) (*model.AccountPortfolio, error) {
	filter := bson.M{
		"injective_address": address.String(),
	}

	opts := &options.FindOneOptions{}
	opts.SetSort(bson.M{"updated_at": -1})

	singleRow := s.accountPortfolioCollection.FindOne(ctx, filter, opts)
	if err := singleRow.Err(); err != nil {
		return nil, err
	}

	var portfolio model.AccountPortfolio
	if err := singleRow.Decode(&portfolio); err != nil {
		return nil, err
	}

	return &portfolio, nil
}

func (s *MongoImpl) ListAccountPortfolios(
	ctx context.Context,
	filter model.AccountPortfoliosFilter,
) (result []*model.AccountPortfolio, err error) {
	portfolioFilter := bson.M{
		"injective_address": filter.InjectiveAddress.String(),
	}

	var updatedAtFilter = make(bson.M)
	if filter.StartTime != nil {
		updatedAtFilter["$gte"] = *filter.StartTime
	}

	if filter.EndTime != nil {
		updatedAtFilter["$lt"] = *filter.EndTime
	}

	if len(updatedAtFilter) > 0 {
		portfolioFilter["updated_at"] = updatedAtFilter
	}

	opts := &options.FindOptions{}
	opts.SetSort(bson.M{"updated_at": -1})

	cur, err := s.accountPortfolioCollection.Find(ctx, portfolioFilter, opts)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	for cur.Next(ctx) {
		var portfolio model.AccountPortfolio
		err := cur.Decode(&portfolio)
		if err != nil {
			return nil, err
		}

		result = append(result, &portfolio)
	}
	return result, nil
}

// AddAccountPortfolios add portfolio snapshots in single write call
func (s *MongoImpl) AddAccountPortfolios(
	ctx context.Context,
	portfolios []*model.AccountPortfolio,
) error {
	docs := make([]interface{}, len(portfolios))
	for i, p := range portfolios {
		docs[i] = p
	}

	_, err := s.accountPortfolioCollection.InsertMany(ctx, docs)
	return err
}

func (s *MongoImpl) Disconnect(ctx context.Context) error {
	return s.client.Disconnect(ctx)
}

func (s *MongoImpl) GetClient() *mongo.Client {
	return s.client
}