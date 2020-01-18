package env

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"

	"github.com/pbarker/go-rl/pkg/v1/common"

	"github.com/ory/dockertest"
	"github.com/pbarker/logger"
	sphere "github.com/pbarker/sphere/api/gen/go/v1alpha"
	"github.com/skratchdot/open-golang/open"
	"google.golang.org/grpc"
	"gorgonia.org/tensor"
)

// Server of environments.
type Server struct {
	// Resource is the underlying docker container.
	Resource *dockertest.Resource

	// Client to connect to the Sphere server.
	Client sphere.EnvironmentAPIClient
}

// ServerConfig is the environment server config.
type ServerConfig struct {
	// Docker image of environment.
	Image string

	// Version of the docker image.
	Version string

	// Port the environment is exposed on.
	Port string
}

// GymServerConfig is a configuration for a OpenAI Gym server environment.
var GymServerConfig = &ServerConfig{Image: "sphereproject/gym", Version: "latest", Port: "50051/tcp"}

// NewLocalServer creates a new environment server by launching a docker container and connecting to it.
func NewLocalServer(config *ServerConfig) (*Server, error) {
	logger.Info("creating local server")
	pool, err := dockertest.NewPool("")
	if err != nil {
		return nil, fmt.Errorf("Could not connect to docker: %s", err)
	}

	resource, err := pool.Run(config.Image, config.Version, []string{})
	if err != nil {
		return nil, fmt.Errorf("Could not start resource: %s", err)
	}

	var sphereClient sphere.EnvironmentAPIClient

	// exponential backoff-retry, because the application in the container might
	// not be ready to accept connections yet
	if err := pool.Retry(func() error {
		var err error
		address := fmt.Sprintf("localhost:%s", resource.GetPort(config.Port))
		conn, err := grpc.Dial(address, grpc.WithInsecure(), grpc.WithBlock())
		if err != nil {
			return err
		}
		sphereClient = sphere.NewEnvironmentAPIClient(conn)
		resp, err := sphereClient.Info(context.Background(), &sphere.Empty{})
		logger.Successf("connected to server %q", resp.ServerName)
		return err
	}); err != nil {
		return nil, fmt.Errorf("Could not connect to docker: %s", err)
	}

	return &Server{
		Resource: resource,
		Client:   sphereClient,
	}, nil
}

// Env is a convienience environment wrapper.
type Env struct {
	*sphere.Environment

	// Client to connect to the Sphere server.
	Client sphere.EnvironmentAPIClient

	// VideoPaths of result videos downloadloaded from the server.
	VideoPaths []string

	// Normalizer normalizes observation data.
	Normalizer Normalizer
}

// Opt is an environment option.
type Opt func(*Env)

// Make an environment.
func (s *Server) Make(model string, opts ...Opt) (*Env, error) {
	ctx := context.Background()
	resp, err := s.Client.CreateEnv(ctx, &sphere.CreateEnvRequest{ModelName: model})
	if err != nil {
		return nil, err
	}
	env := resp.Environment
	logger.Successf("created env: %s", env.Id)
	rresp, err := s.Client.StartRecordEnv(ctx, &sphere.StartRecordEnvRequest{Id: env.Id})
	if err != nil {
		return nil, err
	}
	logger.Success(rresp.Message)
	e := &Env{
		Environment: env,
		Client:      s.Client,
	}
	for _, opt := range opts {
		opt(e)
	}
	return e, nil
}

// WithNormalizer adds a normalizer for observation data.
func WithNormalizer(normalizer Normalizer) func(*Env) {
	return func(e *Env) {
		normalizer.Init(e)
		e.Normalizer = normalizer
	}
}

// Outcome of taking an action.
type Outcome struct {
	// Observation of the current state.
	Observation *tensor.Dense

	// Action that was taken
	Action int

	// Reward from action.
	Reward float32

	// Whether the environment is done.
	Done bool
}

// Step through the environment.
func (e *Env) Step(value int) (*Outcome, error) {
	ctx := context.Background()
	resp, err := e.Client.StepEnv(ctx, &sphere.StepEnvRequest{Id: e.Id, Action: int32(value)})
	if err != nil {
		return nil, err
	}
	observation := resp.Observation.Dense()
	if e.Normalizer != nil {
		observation = e.Normalizer.Norm(observation)
	}
	return &Outcome{observation, value, resp.Reward, resp.Done}, nil
}

// SampleAction returns a sample action for the environment.
func (e *Env) SampleAction() (int, error) {
	ctx := context.Background()
	resp, err := e.Client.SampleAction(ctx, &sphere.SampleActionRequest{Id: e.Id})
	if err != nil {
		return 0, err
	}
	return int(resp.Value), nil
}

// Reset the environment.
func (e *Env) Reset() (observation *tensor.Dense, err error) {
	ctx := context.Background()
	resp, err := e.Client.ResetEnv(ctx, &sphere.ResetEnvRequest{Id: e.Id})
	if err != nil {
		return nil, err
	}
	t := resp.Observation.Dense()
	if e.Normalizer != nil {
		observation = e.Normalizer.Norm(t)
	}
	return t, nil
}

// Close the environment.
func (e *Env) Close() error {
	ctx := context.Background()
	resp, err := e.Client.DeleteEnv(ctx, &sphere.DeleteEnvRequest{Id: e.Id})
	if err != nil {
		return err
	}
	logger.Success(resp.Message)
	return nil
}

// Results from an environment run.
type Results struct {
	// Episodes is a map of episode id to result.
	Episodes map[int32]*sphere.EpisodeResult

	// Videos is a map of episode id to result.
	Videos map[int32]*sphere.Video

	// AverageReward is the average reward of the episodes.
	AverageReward float32
}

// Results results for the environment.
func (e *Env) Results() (*Results, error) {
	ctx := context.Background()
	resp, err := e.Client.Results(ctx, &sphere.ResultsRequest{Id: e.Id})
	if err != nil {
		return nil, err
	}
	var cumulative float32
	for _, res := range resp.EpisodeResults {
		cumulative += res.Reward
	}
	avg := cumulative / float32(len(resp.EpisodeResults))
	res := &Results{
		Episodes:      resp.EpisodeResults,
		Videos:        resp.Videos,
		AverageReward: avg,
	}
	return res, nil
}

// PrintResults results for the environment.
func (e *Env) PrintResults() error {
	results, err := e.Results()
	if err != nil {
		return err
	}
	logger.Infoy("results", results)
	return nil
}

// Videos saves all the videos for the environment episodes to the given path.
// Defaults to current directory. Returns an array of video paths.
func (e *Env) Videos(path string) ([]string, error) {
	if path == "" {
		path = fmt.Sprintf("./results/%s", e.Id)
	}
	ctx := context.Background()
	results, err := e.Results()
	if err != nil {
		return nil, err
	}
	videoPaths := []string{}
	for _, video := range results.Videos {
		stream, err := e.Client.GetVideo(ctx, &sphere.GetVideoRequest{Id: e.Id, EpisodeId: video.EpisodeId})
		if err != nil {
			return nil, err
		}
		fp := filepath.Join(path, fmt.Sprintf("%s-episode%d.mp4", e.Id, video.EpisodeId))
		f, err := os.Create(fp)
		if err != nil {
			return nil, err
		}
		defer f.Close()

		for {
			resp, err := stream.Recv()
			if err == io.EOF {
				err := stream.CloseSend()
				if err != nil {
					return nil, err
				}
				break
			}
			if err != nil {
				return nil, err
			}
			_, err = f.Write(resp.Chunk)
			if err != nil {
				return nil, err
			}
		}
		videoPaths = append(videoPaths, fp)
	}
	e.VideoPaths = videoPaths
	return videoPaths, nil
}

// End is a helper function that will close an environment and return the
// results and play any videos.
func (e *Env) End() {
	err := e.PrintResults()
	if err != nil {
		log.Fatal(err)
	}
	dir, err := ioutil.TempDir("", "sphere")
	if err != nil {
		log.Fatal(err)
	}
	videoPaths, err := e.Videos(dir)
	if err != nil {
		log.Fatal(err)
	}
	logger.Successy("saved videos", videoPaths)
	err = e.Close()
	if err != nil {
		log.Fatal(err)
	}
}

// PlayAll videos stored locally.
func (e *Env) PlayAll() {
	for _, video := range e.VideoPaths {
		logger.Debugf("playing video: %s", video)
		err := open.Run(video)
		if err != nil {
			log.Fatal(err)
		}
	}
	fmt.Print("\npress any key to remove videos or ctrl+c to exit and keep\n")
	input := bufio.NewScanner(os.Stdin)
	input.Scan()
	e.Clean()
}

// Clean any results/videos saved locally.
func (e *Env) Clean() {
	for _, videoPath := range e.VideoPaths {
		err := os.Remove(videoPath)
		if err != nil {
			log.Fatal(err)
		}
		logger.Debugf("removed video: %s", videoPath)
	}
	logger.Success("removed all local videos")
}

// ActionSpaceShape is the shape of the action space.
// TODO: should this be in the API of off the generated code?
func (e *Env) ActionSpaceShape() []int {
	return SpaceShape(e.ActionSpace)
}

// ObservationSpaceShape is the shape of the observation space.
func (e *Env) ObservationSpaceShape() []int {
	return SpaceShape(e.ObservationSpace)
}

// SpaceShape return the shape of the given space.
func SpaceShape(space *sphere.Space) []int {
	shape := []int{}
	switch s := space.GetInfo().(type) {
	case *sphere.Space_Box:
		shape = common.Int32SliceToInt(s.Box.GetShape())
	case *sphere.Space_Discrete:
		shape = []int{1}
	case *sphere.Space_MultiDiscrete:
		shape = []int{len(s.MultiDiscrete.DiscreteSpaces)}
	case *sphere.Space_StructSpace:
		logger.Fatalf("struct space not supported")
	default:
		logger.Fatalf("unknown action space type: %v", space)
	}
	if len(shape) == 0 {
		logger.Fatalf("space had no shape: %v", space)
	}
	return shape
}

// PotentialsShape is an overloaded method that will return a dense tensor of potentials for a given space.
func PotentialsShape(space *sphere.Space) []int {
	shape := []int{}
	switch s := space.GetInfo().(type) {
	case *sphere.Space_Box:
		shape = common.Int32SliceToInt(s.Box.GetShape())
	case *sphere.Space_Discrete:
		shape = []int{int(s.Discrete.N)}
	case *sphere.Space_MultiDiscrete:
		shape = common.Int32SliceToInt(s.MultiDiscrete.DiscreteSpaces)
	case *sphere.Space_StructSpace:
		logger.Fatalf("struct space not supported")
	default:
		logger.Fatalf("unknown action space type: %v", space)
	}
	if len(shape) == 0 {
		logger.Fatalf("space had no shape: %v", space)
	}
	return shape
}

// BoxSpace is a helper for box spaces that converts the values to dense tensors.
// TODO: make proto plugin to do this automagically (protoc-gen-tensor)
type BoxSpace struct {
	// High values for this space.
	High *tensor.Dense

	// Low values for this space.
	Low *tensor.Dense

	// Shape of the space.
	Shape []int
}

// BoxSpace returns the box space as dense tensors.
// TODO: make proto plugin to do this automagically (protoc-gen-tensor)
func (e *Env) BoxSpace() (*BoxSpace, error) {
	space := e.GetObservationSpace()

	if sp := space.GetBox(); sp != nil {
		shape := []int{}
		for _, i := range sp.GetShape() {
			shape = append(shape, int(i))
		}
		return &BoxSpace{
			High:  tensor.New(tensor.WithShape(shape...), tensor.WithBacking(sp.GetHigh())),
			Low:   tensor.New(tensor.WithShape(shape...), tensor.WithBacking(sp.GetLow())),
			Shape: shape,
		}, nil
	}
	return nil, fmt.Errorf("env is not a box space: %+v", space)
}

// Print a YAML representation of the environment.
func (e *Env) Print() {
	logger.Infoy("environment", e.Environment)
}
