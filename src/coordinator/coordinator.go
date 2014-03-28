package coordinator

import (
	"cluster"
	"common"
	"configuration"
	"engine"
	"fmt"
	"math"
	"os"
	"parser"
	"protocol"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	log "code.google.com/p/log4go"
)

type CoordinatorImpl struct {
	clusterConfiguration *cluster.ClusterConfiguration
	raftServer           ClusterConsensus
	config               *configuration.Configuration
}

const (
	// this is the key used for the persistent atomic ints for sequence numbers
	POINT_SEQUENCE_NUMBER_KEY = "p"

	// actual point sequence numbers will have the first part of the number
	// be a host id. This ensures that sequence numbers are unique across the cluster
	HOST_ID_OFFSET = uint64(10000)

	SHARDS_TO_QUERY_FOR_LIST_SERIES = 10
)

var (
	BARRIER_TIME_MIN int64 = math.MinInt64
	BARRIER_TIME_MAX int64 = math.MaxInt64
)

// shorter constants for readability
var (
	dropDatabase         = protocol.Request_DROP_DATABASE
	queryRequest         = protocol.Request_QUERY
	endStreamResponse    = protocol.Response_END_STREAM
	queryResponse        = protocol.Response_QUERY
	heartbeatResponse    = protocol.Response_HEARTBEAT
	explainQueryResponse = protocol.Response_EXPLAIN_QUERY
	write                = protocol.Request_WRITE
)

type SeriesWriter interface {
	Write(*protocol.Series) error
	Close()
}

// usernames and db names should match this regex
var VALID_NAMES *regexp.Regexp

func init() {
	var err error
	VALID_NAMES, err = regexp.Compile("^[a-zA-Z0-9_][a-zA-Z0-9\\._-]*$")
	if err != nil {
		panic(err)
	}
}

func NewCoordinatorImpl(config *configuration.Configuration, raftServer ClusterConsensus, clusterConfiguration *cluster.ClusterConfiguration) *CoordinatorImpl {
	coordinator := &CoordinatorImpl{
		config:               config,
		clusterConfiguration: clusterConfiguration,
		raftServer:           raftServer,
	}

	return coordinator
}

func (self *CoordinatorImpl) RunQuery(user common.User, database string, queryString string, seriesWriter SeriesWriter) (err error) {
	log.Debug("COORD: RunQuery: %s", queryString)
	// don't let a panic pass beyond RunQuery
	defer recoverFunc(database, queryString)

	q, err := parser.ParseQuery(queryString)
	if err != nil {
		return err
	}

	for _, query := range q {
		querySpec := parser.NewQuerySpec(user, database, query)

		if query.DeleteQuery != nil {
			if err := self.runDeleteQuery(querySpec, seriesWriter); err != nil {
				return err
			}
			continue
		}

		if query.DropQuery != nil {
			if err := self.DeleteContinuousQuery(user, database, uint32(query.DropQuery.Id)); err != nil {
				return err
			}
			continue
		}

		if query.IsListQuery() {
			if query.IsListSeriesQuery() {
				self.runListSeriesQuery(querySpec, seriesWriter)
			} else if query.IsListContinuousQueriesQuery() {
				queries, err := self.ListContinuousQueries(user, database)
				if err != nil {
					return err
				}
				for _, q := range queries {
					if err := seriesWriter.Write(q); err != nil {
						return err
					}
				}
			}
			continue
		}

		if query.DropSeriesQuery != nil {
			err := self.runDropSeriesQuery(querySpec, seriesWriter)
			if err != nil {
				return err
			}
			continue
		}

		selectQuery := query.SelectQuery

		if selectQuery.IsContinuousQuery() {
			return self.CreateContinuousQuery(user, database, queryString)
		}

		return self.runQuery(query, user, database, seriesWriter)
	}
	seriesWriter.Close()
	return nil
}

// This should only get run for SelectQuery types
func (self *CoordinatorImpl) runQuery(query *parser.Query, user common.User, database string, seriesWriter SeriesWriter) error {
	querySpec := parser.NewQuerySpec(user, database, query)
	return self.runQuerySpec(querySpec, seriesWriter)
}

func (self *CoordinatorImpl) runListSeriesQuery(querySpec *parser.QuerySpec, seriesWriter SeriesWriter) error {
	shortTermShards := self.clusterConfiguration.GetShortTermShards()
	if len(shortTermShards) > SHARDS_TO_QUERY_FOR_LIST_SERIES {
		shortTermShards = shortTermShards[:SHARDS_TO_QUERY_FOR_LIST_SERIES]
	}
	longTermShards := self.clusterConfiguration.GetLongTermShards()
	if len(longTermShards) > SHARDS_TO_QUERY_FOR_LIST_SERIES {
		longTermShards = longTermShards[:SHARDS_TO_QUERY_FOR_LIST_SERIES]
	}
	seriesYielded := make(map[string]bool)

	shards := append(shortTermShards, longTermShards...)

	var err error
	for _, shard := range shards {
		responseChan := make(chan *protocol.Response, shard.QueryResponseBufferSize(querySpec, self.config.LevelDbPointBatchSize))
		go shard.Query(querySpec, responseChan)
		for {
			response := <-responseChan
			if *response.Type == endStreamResponse || *response.Type == accessDeniedResponse {
				if response.ErrorMessage != nil && err != nil {
					log.Debug("Error when querying shard: %s", err)
					err = common.NewQueryError(common.InvalidArgument, *response.ErrorMessage)
				}
				break
			}
			for _, series := range response.MultiSeries {
				if !seriesYielded[*series.Name] {
					seriesYielded[*series.Name] = true
					seriesWriter.Write(series)
				}
			}
		}
	}
	seriesWriter.Close()
	return err
}

func (self *CoordinatorImpl) runDeleteQuery(querySpec *parser.QuerySpec, seriesWriter SeriesWriter) error {
	db := querySpec.Database()
	if !querySpec.User().IsDbAdmin(db) {
		return common.NewAuthorizationError("Insufficient permission to write to %s", db)
	}
	querySpec.RunAgainstAllServersInShard = true
	return self.runQuerySpec(querySpec, seriesWriter)
}

func (self *CoordinatorImpl) runDropSeriesQuery(querySpec *parser.QuerySpec, seriesWriter SeriesWriter) error {
	user := querySpec.User()
	db := querySpec.Database()
	series := querySpec.Query().DropSeriesQuery.GetTableName()
	if !user.IsClusterAdmin() && !user.IsDbAdmin(db) && !user.HasWriteAccess(series) {
		return common.NewAuthorizationError("Insufficient permissions to drop series")
	}
	querySpec.RunAgainstAllServersInShard = true
	return self.runQuerySpec(querySpec, seriesWriter)
}

func (self *CoordinatorImpl) shouldAggregateLocally(shards []*cluster.ShardData, querySpec *parser.QuerySpec) bool {
	for _, s := range shards {
		if !s.ShouldAggregateLocally(querySpec) {
			return false
		}
	}
	return true
}

func (self *CoordinatorImpl) shouldQuerySequentially(shards []*cluster.ShardData, querySpec *parser.QuerySpec) bool {
	// if we're not aggregating locally, that means all the raw points are being sent back in this query. Do it
	// sequentially so we don't fill up memory like crazy.
	return !self.shouldAggregateLocally(shards, querySpec)
}

func (self *CoordinatorImpl) getShardsAndProcessor(querySpec *parser.QuerySpec, writer SeriesWriter) ([]*cluster.ShardData, cluster.QueryProcessor, chan bool, error) {
	shards := self.clusterConfiguration.GetShards(querySpec)
	shouldAggregateLocally := self.shouldAggregateLocally(shards, querySpec)

	var err error
	var processor cluster.QueryProcessor

	responseChan := make(chan *protocol.Response)
	seriesClosed := make(chan bool)

	selectQuery := querySpec.SelectQuery()
	if selectQuery != nil && !shouldAggregateLocally {
		// if we should aggregate in the coordinator (i.e. aggregation
		// isn't happening locally at the shard level), create an engine
		processor, err = engine.NewQueryEngine(querySpec.SelectQuery(), responseChan)
	} else if selectQuery != nil && selectQuery.Limit > 0 {
		// if we have a query with limit, then create an engine, or we can
		// make the passthrough limit aware
		processor = engine.NewPassthroughEngineWithLimit(responseChan, 100, selectQuery.Limit)
	} else if !shouldAggregateLocally {
		processor = engine.NewPassthroughEngine(responseChan, 100)
	}

	if err != nil {
		return nil, nil, nil, err
	}

	if processor == nil {
		return shards, nil, nil, nil
	}

	go func() {
		for {
			response := <-responseChan

			if *response.Type == endStreamResponse || *response.Type == accessDeniedResponse {
				writer.Close()
				seriesClosed <- true
				return
			}
			if !(*response.Type == queryResponse && querySpec.IsExplainQuery()) {
				if response.Series != nil && len(response.Series.Points) > 0 {
					writer.Write(response.Series)
				}
			}
		}
	}()

	return shards, processor, seriesClosed, nil
}

func (self *CoordinatorImpl) runQuerySpec(querySpec *parser.QuerySpec, seriesWriter SeriesWriter) error {
	shards, processor, seriesClosed, err := self.getShardsAndProcessor(querySpec, seriesWriter)
	if err != nil {
		return err
	}

	responses := make([]chan *protocol.Response, len(shards), len(shards))

	shardConcurrentLimit := self.config.ConcurrentShardQueryLimit
	if self.shouldQuerySequentially(shards, querySpec) {
		log.Debug("Querying shards sequentially")
		shardConcurrentLimit = 1
	}
	log.Debug("Shard concurrent limit: ", shardConcurrentLimit)
	for i := 0; i < shardConcurrentLimit && i < len(shards); i++ {
		shard := shards[i]
		responseChan := make(chan *protocol.Response, shard.QueryResponseBufferSize(querySpec, self.config.LevelDbPointBatchSize))
		// We query shards for data and stream them to query processor
		go shard.Query(querySpec, responseChan)
		responses[i] = responseChan
	}
	nextIndex := shardConcurrentLimit
	// don't queue up new shards to query if we've hit the limit for the query
	shouldContinue := false

	for i, responseChan := range responses {
		log.Debug("READING: shard: ", i, shards[i].String())

		// Do this because it's possible should continue was false so we haven't set the other response channels.
		if responseChan == nil {
			break
		}
		for {
			response := <-responseChan

			//log.Debug("GOT RESPONSE: ", response.Type, response.Series)
			log.Debug("GOT RESPONSE: ", response.Type)
			if *response.Type == endStreamResponse || *response.Type == accessDeniedResponse {
				if response.ErrorMessage != nil && err == nil {
					err = common.NewQueryError(common.InvalidArgument, *response.ErrorMessage)
				}
				if nextIndex < len(shards) && shouldContinue {
					shard := shards[nextIndex]
					responseChan := make(chan *protocol.Response, shard.QueryResponseBufferSize(querySpec, self.config.LevelDbPointBatchSize))
					// We query shards for data and stream them to query processor
					log.Debug("Querying Shard: ", nextIndex, shard.String())
					go shard.Query(querySpec, responseChan)
					responses[nextIndex] = responseChan
					nextIndex += 1
				}
				break
			}

			if response.Series == nil || len(response.Series.Points) == 0 {
				log.Debug("Series has no points, continue")
				continue
			}

			// if we don't have a processor, yield the point to the writer
			// this happens if shard took care of the query
			// otherwise client will get points from passthrough engine
			if processor != nil {
				// if the data wasn't aggregated at the shard level, aggregate
				// the data here
				log.Debug("YIELDING: %d points with %d columns", len(response.Series.Points), len(response.Series.Fields))
				shouldContinue = processor.YieldSeries(response.Series)
				log.Debug("ShouldContinue: ", shouldContinue)
				continue
			}

			// If we have EXPLAIN query, we don't write actual points (of
			// response.Type Query) to the client
			if !(*response.Type == queryResponse && querySpec.IsExplainQuery()) {
				seriesWriter.Write(response.Series)
			}
		}
		log.Debug("DONE: shard: ", shards[i].String())
	}

	if processor != nil {
		processor.Close()
		<-seriesClosed
		return err
	}
	seriesWriter.Close()
	return err
}

func recoverFunc(database, query string) {
	if err := recover(); err != nil {
		fmt.Fprintf(os.Stderr, "********************************BUG********************************\n")
		buf := make([]byte, 1024)
		n := runtime.Stack(buf, false)
		fmt.Fprintf(os.Stderr, "Database: %s\n", database)
		fmt.Fprintf(os.Stderr, "Query: [%s]\n", query)
		fmt.Fprintf(os.Stderr, "Error: %s. Stacktrace: %s\n", err, string(buf[:n]))
		err = common.NewQueryError(common.InternalError, "Internal Error")
	}
}

func (self *CoordinatorImpl) ForceCompaction(user common.User) error {
	if !user.IsClusterAdmin() {
		return fmt.Errorf("Insufficient permissions to force a log compaction")
	}

	return self.raftServer.ForceLogCompaction()
}

func (self *CoordinatorImpl) WriteSeriesData(user common.User, db string, series *protocol.Series) error {
	if !user.HasWriteAccess(db) {
		return common.NewAuthorizationError("Insufficient permissions to write to %s", db)
	}
	if len(series.Points) == 0 {
		return fmt.Errorf("Can't write series with zero points.")
	}

	err := self.CommitSeriesData(db, series)
	if err != nil {
		return err
	}

	self.ProcessContinuousQueries(db, series)

	return err
}

func (self *CoordinatorImpl) ProcessContinuousQueries(db string, series *protocol.Series) {
	if self.clusterConfiguration.ParsedContinuousQueries != nil {
		incomingSeriesName := *series.Name
		for _, query := range self.clusterConfiguration.ParsedContinuousQueries[db] {
			groupByClause := query.GetGroupByClause()
			if groupByClause.Elems != nil {
				continue
			}

			fromClause := query.GetFromClause()
			intoClause := query.GetIntoClause()
			targetName := intoClause.Target.Name

			for _, table := range fromClause.Names {
				tableValue := table.Name
				if regex, ok := tableValue.GetCompiledRegex(); ok {
					if regex.MatchString(incomingSeriesName) {
						self.InterpolateValuesAndCommit(db, series, targetName, false)
					}
				} else {
					if tableValue.Name == incomingSeriesName {
						self.InterpolateValuesAndCommit(db, series, targetName, false)
					}
				}
			}
		}
	}
}

func (self *CoordinatorImpl) InterpolateValuesAndCommit(db string, series *protocol.Series, targetName string, assignSequenceNumbers bool) error {
	targetName = strings.Replace(targetName, ":series_name", *series.Name, -1)
	type sequenceKey struct {
		seriesName string
		timestamp  int64
	}
	sequenceMap := make(map[sequenceKey]int)
	r, _ := regexp.Compile(`\[.*?\]`)
	replaceInvalidCharacters := func(r rune) rune {
		switch {
		case (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			return r
		case r == '_' || r == '-' || r == '.':
			return r
		case r == ' ':
			return '_'
		case r == '/':
			return '.'
		}
		return -1
	}

	if r.MatchString(targetName) {
		for _, point := range series.Points {
			targetNameWithValues := r.ReplaceAllStringFunc(targetName, func(match string) string {
				fieldName := match[1 : len(match)-1]
				fieldIndex := series.GetFieldIndex(fieldName)
				return point.GetFieldValueAsString(fieldIndex)
			})
			cleanedTargetName := strings.Map(replaceInvalidCharacters, targetNameWithValues)

			if assignSequenceNumbers {
				sequenceMap[sequenceKey{targetName, *point.Timestamp}] += 1
				sequenceNumber := uint64(sequenceMap[sequenceKey{targetName, *point.Timestamp}])
				point.SequenceNumber = &sequenceNumber
			}

			newSeries := &protocol.Series{Name: &cleanedTargetName, Fields: series.Fields, Points: []*protocol.Point{point}}
			if e := self.CommitSeriesData(db, newSeries); e != nil {
				log.Error("Couldn't write data for continuous query: ", e)
			}
		}
	} else {
		newSeries := &protocol.Series{Name: &targetName, Fields: series.Fields, Points: series.Points}

		if assignSequenceNumbers {
			for _, point := range newSeries.Points {
				sequenceMap[sequenceKey{targetName, *point.Timestamp}] += 1
				sequenceNumber := uint64(sequenceMap[sequenceKey{targetName, *point.Timestamp}])
				point.SequenceNumber = &sequenceNumber
			}
		}

		if e := self.CommitSeriesData(db, newSeries); e != nil {
			log.Error("Couldn't write data for continuous query: ", e)
		}
	}

	return nil
}

func (self *CoordinatorImpl) CommitSeriesData(db string, series *protocol.Series) error {
	lastTime := int64(0)
	lastPointIndex := 0
	now := common.CurrentTime()
	var shardToWrite cluster.Shard
	for _, point := range series.Points {
		if point.Timestamp == nil {
			point.Timestamp = &now
		}
	}

	// sort the points by timestamp
	series.SortPointsTimeDescending()

	for i, point := range series.Points {
		if *point.Timestamp != lastTime {
			shard, err := self.clusterConfiguration.GetShardToWriteToBySeriesAndTime(db, *series.Name, *point.Timestamp)
			if err != nil {
				return err
			}
			if shardToWrite == nil {
				shardToWrite = shard
			} else if shardToWrite.Id() != shard.Id() {
				newIndex := i
				newSeries := &protocol.Series{Name: series.Name, Fields: series.Fields, Points: series.Points[lastPointIndex:newIndex]}
				if err := self.write(db, newSeries, shardToWrite); err != nil {
					return err
				}
				lastPointIndex = newIndex
				shardToWrite = shard
			}
			lastTime = *point.Timestamp
		}
	}

	series.Points = series.Points[lastPointIndex:]

	if len(series.Points) > 0 {
		if shardToWrite == nil {
			shardToWrite, _ = self.clusterConfiguration.GetShardToWriteToBySeriesAndTime(db, *series.Name, *series.Points[0].Timestamp)
		}

		err := self.write(db, series, shardToWrite)

		if err != nil {
			log.Error("COORD error writing: ", err)
			return err
		}

		return err
	}

	return nil
}

func (self *CoordinatorImpl) write(db string, series *protocol.Series, shard cluster.Shard) error {
	request := &protocol.Request{Type: &write, Database: &db, Series: series}
	return shard.Write(request)
}

func (self *CoordinatorImpl) CreateContinuousQuery(user common.User, db string, query string) error {
	if !user.IsClusterAdmin() && !user.IsDbAdmin(db) {
		return common.NewAuthorizationError("Insufficient permissions to create continuous query")
	}

	err := self.raftServer.CreateContinuousQuery(db, query)
	if err != nil {
		return err
	}
	return nil
}

func (self *CoordinatorImpl) DeleteContinuousQuery(user common.User, db string, id uint32) error {
	if !user.IsClusterAdmin() && !user.IsDbAdmin(db) {
		return common.NewAuthorizationError("Insufficient permissions to delete continuous query")
	}

	err := self.raftServer.DeleteContinuousQuery(db, id)
	if err != nil {
		return err
	}
	return nil
}

func (self *CoordinatorImpl) ListContinuousQueries(user common.User, db string) ([]*protocol.Series, error) {
	if !user.IsClusterAdmin() && !user.IsDbAdmin(db) {
		return nil, common.NewAuthorizationError("Insufficient permissions to list continuous queries")
	}

	queries := self.clusterConfiguration.GetContinuousQueries(db)
	points := []*protocol.Point{}

	for _, query := range queries {
		queryId := int64(query.Id)
		queryString := query.Query
		timestamp := time.Now().Unix()
		sequenceNumber := uint64(1)
		points = append(points, &protocol.Point{
			Values: []*protocol.FieldValue{
				&protocol.FieldValue{Int64Value: &queryId},
				&protocol.FieldValue{StringValue: &queryString},
			},
			Timestamp:      &timestamp,
			SequenceNumber: &sequenceNumber,
		})
	}
	seriesName := "continuous queries"
	series := []*protocol.Series{&protocol.Series{
		Name:   &seriesName,
		Fields: []string{"id", "query"},
		Points: points,
	}}
	return series, nil
}

func (self *CoordinatorImpl) CreateDatabase(user common.User, db string, replicationFactor uint8) error {
	if !user.IsClusterAdmin() {
		return common.NewAuthorizationError("Insufficient permissions to create database")
	}

	if !isValidName(db) {
		return fmt.Errorf("%s isn't a valid db name", db)
	}

	err := self.raftServer.CreateDatabase(db, replicationFactor)
	if err != nil {
		return err
	}
	return nil
}

func (self *CoordinatorImpl) ListDatabases(user common.User) ([]*cluster.Database, error) {
	if !user.IsClusterAdmin() {
		return nil, common.NewAuthorizationError("Insufficient permissions to list databases")
	}

	dbs := self.clusterConfiguration.GetDatabases()
	return dbs, nil
}

func (self *CoordinatorImpl) DropDatabase(user common.User, db string) error {
	if !user.IsClusterAdmin() {
		return common.NewAuthorizationError("Insufficient permissions to drop database")
	}

	if err := self.clusterConfiguration.CreateCheckpoint(); err != nil {
		return err
	}

	if err := self.raftServer.DropDatabase(db); err != nil {
		return err
	}

	var wait sync.WaitGroup
	for _, shard := range self.clusterConfiguration.GetAllShards() {
		wait.Add(1)
		go func(shard *cluster.ShardData) {
			shard.DropDatabase(db, true)
			wait.Done()
		}(shard)
	}
	wait.Wait()
	return nil
}

func (self *CoordinatorImpl) AuthenticateDbUser(db, username, password string) (common.User, error) {
	log.Debug("(raft:%s) Authenticating password for %s:%s", self.raftServer.(*RaftServer).raftServer.Name(), db, username)
	user, err := self.clusterConfiguration.AuthenticateDbUser(db, username, password)
	if user != nil {
		log.Debug("(raft:%s) User %s authenticated succesfuly", self.raftServer.(*RaftServer).raftServer.Name(), username)
	}
	return user, err
}

func (self *CoordinatorImpl) AuthenticateClusterAdmin(username, password string) (common.User, error) {
	return self.clusterConfiguration.AuthenticateClusterAdmin(username, password)
}

func (self *CoordinatorImpl) ListClusterAdmins(requester common.User) ([]string, error) {
	if !requester.IsClusterAdmin() {
		return nil, common.NewAuthorizationError("Insufficient permissions")
	}

	return self.clusterConfiguration.GetClusterAdmins(), nil
}

func (self *CoordinatorImpl) CreateClusterAdminUser(requester common.User, username, password string) error {
	if !requester.IsClusterAdmin() {
		return common.NewAuthorizationError("Insufficient permissions")
	}

	if !isValidName(username) {
		return fmt.Errorf("%s isn't a valid username", username)
	}

	hash, err := cluster.HashPassword(password)
	if err != nil {
		return err
	}

	if self.clusterConfiguration.GetClusterAdmin(username) != nil {
		return fmt.Errorf("User %s already exists", username)
	}

	return self.raftServer.SaveClusterAdminUser(&cluster.ClusterAdmin{cluster.CommonUser{Name: username, CacheKey: username, Hash: string(hash)}})
}

func (self *CoordinatorImpl) DeleteClusterAdminUser(requester common.User, username string) error {
	if !requester.IsClusterAdmin() {
		return common.NewAuthorizationError("Insufficient permissions")
	}

	user := self.clusterConfiguration.GetClusterAdmin(username)
	if user == nil {
		return fmt.Errorf("User %s doesn't exists", username)
	}

	user.CommonUser.IsUserDeleted = true
	return self.raftServer.SaveClusterAdminUser(user)
}

func (self *CoordinatorImpl) ChangeClusterAdminPassword(requester common.User, username, password string) error {
	if !requester.IsClusterAdmin() {
		return common.NewAuthorizationError("Insufficient permissions")
	}

	user := self.clusterConfiguration.GetClusterAdmin(username)
	if user == nil {
		return fmt.Errorf("Invalid user name %s", username)
	}

	hash, err := cluster.HashPassword(password)
	if err != nil {
		return err
	}
	user.ChangePassword(string(hash))
	return self.raftServer.SaveClusterAdminUser(user)
}

func (self *CoordinatorImpl) CreateDbUser(requester common.User, db, username, password string) error {
	if !requester.IsClusterAdmin() && !requester.IsDbAdmin(db) {
		return common.NewAuthorizationError("Insufficient permissions")
	}

	if username == "" {
		return fmt.Errorf("Username cannot be empty")
	}

	if !isValidName(username) {
		return fmt.Errorf("%s isn't a valid username", username)
	}

	hash, err := cluster.HashPassword(password)
	if err != nil {
		return err
	}

	self.CreateDatabase(requester, db, uint8(1)) // ignore the error since the db may exist
	if self.clusterConfiguration.GetDbUser(db, username) != nil {
		return fmt.Errorf("User %s already exists", username)
	}
	matchers := []*cluster.Matcher{&cluster.Matcher{true, ".*"}}
	log.Debug("(raft:%s) Creating user %s:%s", self.raftServer.(*RaftServer).raftServer.Name(), db, username)
	return self.raftServer.SaveDbUser(&cluster.DbUser{cluster.CommonUser{
		Name:     username,
		Hash:     string(hash),
		CacheKey: db + "%" + username,
	}, db, matchers, matchers, false})
}

func (self *CoordinatorImpl) DeleteDbUser(requester common.User, db, username string) error {
	if !requester.IsClusterAdmin() && !requester.IsDbAdmin(db) {
		return common.NewAuthorizationError("Insufficient permissions")
	}

	user := self.clusterConfiguration.GetDbUser(db, username)
	if user == nil {
		return fmt.Errorf("User %s doesn't exist", username)
	}
	user.CommonUser.IsUserDeleted = true
	return self.raftServer.SaveDbUser(user)
}

func (self *CoordinatorImpl) ListDbUsers(requester common.User, db string) ([]common.User, error) {
	if !requester.IsClusterAdmin() && !requester.IsDbAdmin(db) {
		return nil, common.NewAuthorizationError("Insufficient permissions")
	}

	return self.clusterConfiguration.GetDbUsers(db), nil
}

func (self *CoordinatorImpl) GetDbUser(requester common.User, db string, username string) (common.User, error) {
	if !requester.IsClusterAdmin() && !requester.IsDbAdmin(db) {
		return nil, common.NewAuthorizationError("Insufficient permissions")
	}

	dbUser := self.clusterConfiguration.GetDbUser(db, username)
	if dbUser == nil {
		return nil, fmt.Errorf("Invalid username %s", username)
	}

	return dbUser, nil
}

func (self *CoordinatorImpl) ChangeDbUserPassword(requester common.User, db, username, password string) error {
	if !requester.IsClusterAdmin() && !requester.IsDbAdmin(db) && !(requester.GetDb() == db && requester.GetName() == username) {
		return common.NewAuthorizationError("Insufficient permissions")
	}

	hash, err := cluster.HashPassword(password)
	if err != nil {
		return err
	}
	return self.raftServer.ChangeDbUserPassword(db, username, hash)
}

func (self *CoordinatorImpl) SetDbAdmin(requester common.User, db, username string, isAdmin bool) error {
	if !requester.IsClusterAdmin() && !requester.IsDbAdmin(db) {
		return common.NewAuthorizationError("Insufficient permissions")
	}

	user := self.clusterConfiguration.GetDbUser(db, username)
	if user == nil {
		return fmt.Errorf("Invalid username %s", username)
	}
	user.IsAdmin = isAdmin
	self.raftServer.SaveDbUser(user)
	return nil
}

func (self *CoordinatorImpl) ConnectToProtobufServers(localConnectionString string) error {
	log.Info("Connecting to other nodes in the cluster")

	for _, server := range self.clusterConfiguration.Servers() {
		if server.ProtobufConnectionString != localConnectionString {
			server.Connect()
		}
	}
	return nil
}

func isValidName(name string) bool {
	return VALID_NAMES.MatchString(name)
}
