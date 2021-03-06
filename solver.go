package elasticthought

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/couchbaselabs/logg"
	"github.com/golang/protobuf/proto"
	"github.com/tleyden/cbfs/client"
	"github.com/tleyden/elastic-thought/caffe"
	"github.com/tleyden/go-couch"
)

// A solver can generate trained models, which ban be used to make predictions
type Solver struct {
	ElasticThoughtDoc
	DatasetId           string `json:"dataset-id"`
	SpecificationUrl    string `json:"specification-url" binding:"required"`
	SpecificationNetUrl string `json:"specification-net-url" binding:"required"`

	// had to make exported, due to https://github.com/gin-gonic/gin/pull/123
	// waiting for this to get merged into master branch, since go get
	// pulls from master branch.
	Configuration Configuration
}

// Create a new solver.  If you don't use this, you must set the
// embedded ElasticThoughtDoc Type field.
func NewSolver(config Configuration) *Solver {
	return &Solver{
		ElasticThoughtDoc: ElasticThoughtDoc{Type: DOC_TYPE_SOLVER},
		Configuration:     config,
	}
}

// Insert into database (only call this if you know it doesn't arleady exist,
// or else you'll end up w/ unwanted dupes)
func (s Solver) Insert(db couch.Database) (*Solver, error) {

	id, _, err := db.Insert(s)
	if err != nil {
		err := fmt.Errorf("Error inserting solver: %v.  Err: %v", s, err)
		return nil, err
	}

	// load dataset object from db (so we have id/rev fields)
	solver := &Solver{}
	err = db.Retrieve(id, solver)
	if err != nil {
		err := fmt.Errorf("Error fetching solver: %v.  Err: %v", id, err)
		return nil, err
	}

	return solver, nil

}

// read solver prototxt from cbfs
func (s Solver) getSolverPrototxtContent() ([]byte, error) {

	// get the relative url path in cbfs (chop off leading cbfs://)
	sourcePath, err := s.SpecificationUrlPath()
	if err != nil {
		return nil, fmt.Errorf("Error getting cbfs path of solver prototxt. Err: %v", err)
	}

	// create a new cbfs client
	cbfs, err := s.Configuration.NewCbfsClient()
	if err != nil {
		return nil, fmt.Errorf("Error creating cbfs client: %v", err)
	}

	return getContentFromCbfs(cbfs, sourcePath)

}

func (s Solver) getSolverParameter() (*caffe.SolverParameter, error) {

	specContents, err := s.getSolverPrototxtContent()
	if err != nil {
		return nil, fmt.Errorf("Error getting solver prototxt content.  Err: %v", err)
	}

	// read into object with protobuf (must have already generated go protobuf code)
	solverParam := &caffe.SolverParameter{}

	if err := proto.UnmarshalText(string(specContents), solverParam); err != nil {
		return nil, err
	}

	return solverParam, nil

}

// download contents of solver-spec-url and make the following modifications:
// - Replace net with "solver-net.prototxt"
// - Replace snapshot_prefix with "snapshot"
func (s Solver) getModifiedSolverSpec() ([]byte, error) {

	// read in spec from url -> []byte
	content, err := getUrlContent(s.SpecificationUrl)
	if err != nil {
		return nil, fmt.Errorf("Error getting data: %v.  %v", s.SpecificationUrl, err)
	}

	// pass in []byte to modifier and get modified []byte
	modified, err := modifySolverSpec(content)
	if err != nil {
		return nil, fmt.Errorf("Error modifying: %v.  %v", string(content), err)
	}

	return modified, nil

}

// download contents of solver-spec-net-url and make the following modifications:
// - Replace layers / image_data_param / source with "train" and "test"
func (s Solver) getModifiedSolverNetSpec() ([]byte, error) {

	// read in spec from url -> []byte
	content, err := getUrlContent(s.SpecificationNetUrl)
	if err != nil {
		return nil, fmt.Errorf("Error getting data: %v.  %v", s.SpecificationNetUrl, err)
	}

	// pass in []byte to modifier and get modified []byte
	modified, err := modifySolverNetSpec(content)
	if err != nil {
		return nil, fmt.Errorf("Error modifying: %v.  %v", string(content), err)
	}

	return modified, nil

}

func modifySolverNetSpec(sourceBytes []byte) ([]byte, error) {

	// read into object with protobuf (must have already generated go protobuf code)
	netParam := &caffe.NetParameter{}

	if err := proto.UnmarshalText(string(sourceBytes), netParam); err != nil {
		return nil, err
	}

	// modify object fields
	for _, layerParam := range netParam.Layers {

		switch *layerParam.Type {
		case caffe.V1LayerParameter_IMAGE_DATA:

			if isTrainingPhase(layerParam) {
				layerParam.ImageDataParam.Source = proto.String(TRAINING_INDEX)
			}
			if isTestingPhase(layerParam) {
				layerParam.ImageDataParam.Source = proto.String(TESTING_INDEX)
			}

		case caffe.V1LayerParameter_DATA:

			if isTrainingPhase(layerParam) {
				layerParam.DataParam.Source = proto.String(TRAINING_DIR)
			}
			if isTestingPhase(layerParam) {
				layerParam.DataParam.Source = proto.String(TESTING_DIR)
			}

		}

	}

	buf := new(bytes.Buffer)
	if err := proto.MarshalText(buf, netParam); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil

}

func isTrainingPhase(layer *caffe.V1LayerParameter) bool {
	for _, includedPhase := range layer.Include {
		if *includedPhase.Phase == caffe.Phase_TRAIN {
			return true
		}
	}
	return false
}

func isTestingPhase(layer *caffe.V1LayerParameter) bool {
	for _, includedPhase := range layer.Include {
		if *includedPhase.Phase == caffe.Phase_TEST {
			return true
		}
	}
	return false
}

func modifySolverSpec(source []byte) ([]byte, error) {

	// read into object with protobuf (must have already generated go protobuf code)
	solverParam := &caffe.SolverParameter{}

	if err := proto.UnmarshalText(string(source), solverParam); err != nil {
		return nil, err
	}

	// modify object fields
	solverParam.Net = proto.String("solver-net.prototxt")
	solverParam.SnapshotPrefix = proto.String("snapshot")

	buf := new(bytes.Buffer)
	if err := proto.MarshalText(buf, solverParam); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil

}

// download contents of solver-spec-url into cbfs://<solver-id>/spec.prototxt
// and update solver object's solver-spec-url with cbfs url
func (s Solver) DownloadSpecToCbfs(db couch.Database, cbfs *cbfsclient.Client) (*Solver, error) {

	// rewrite the solver specification
	solverSpecBytes, err := s.getModifiedSolverSpec()
	if err != nil {
		return nil, err
	}

	// save rewritten solver to cbfs
	destPath := fmt.Sprintf("%v/solver.prototxt", s.Id)
	reader := bytes.NewReader(solverSpecBytes)
	if err := s.saveToCbfs(cbfs, destPath, reader); err != nil {
		return nil, err
	}

	// update solver with cbfs url
	s.SpecificationUrl = fmt.Sprintf("%v%v", CBFS_URI_PREFIX, destPath)

	// TODO: s.MaxIterations =

	// rewrite the solver net specification
	solverSpecNetBytes, err := s.getModifiedSolverNetSpec()
	if err != nil {
		return nil, err
	}

	// save rewritten solver to cbfs
	destPath = fmt.Sprintf("%v/solver-net.prototxt", s.Id)
	reader = bytes.NewReader(solverSpecNetBytes)
	if err := s.saveToCbfs(cbfs, destPath, reader); err != nil {
		return nil, err
	}

	// update solver-net with cbfs url
	s.SpecificationNetUrl = fmt.Sprintf("%v%v", CBFS_URI_PREFIX, destPath)

	// save
	solver, err := s.Save(db)
	if err != nil {
		return nil, err
	}

	return solver, nil
}

func (s Solver) saveToCbfs(cbfs *cbfsclient.Client, destPath string, reader io.Reader) error {

	// save to cbfs
	options := cbfsclient.PutOptions{
		ContentType: "text/plain",
	}

	if err := cbfs.Put("", destPath, reader, options); err != nil {
		return fmt.Errorf("Error writing %v to cbfs: %v", destPath, err)
	}
	logg.LogTo("REST", "Wrote %v to cbfs", destPath)
	return nil

}

func (s Solver) saveUrlToCbfs(cbfs *cbfsclient.Client, destPath, sourceUrl string) error {

	// open stream to source url
	resp, err := http.Get(sourceUrl)
	if err != nil {
		return fmt.Errorf("Error doing GET on: %v.  %v", sourceUrl, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("%v response to GET on: %v", resp.StatusCode, sourceUrl)
	}
	return s.saveToCbfs(cbfs, destPath, resp.Body)

}

// Saves the solver to the db, returns latest rev
func (s Solver) Save(db couch.Database) (*Solver, error) {

	// TODO: retry if 409 error
	_, err := db.Edit(s)
	if err != nil {
		return nil, err
	}

	// load latest version of dataset to return
	solver := &Solver{}
	err = db.Retrieve(s.Id, solver)
	if err != nil {
		return nil, err
	}

	return solver, nil

}

// Save the content in the SpecificationUrl to the given directory.
// As the filename, use the last part of the url path from the SpecificationUrl
func (s Solver) writeSpecToFile(config Configuration, destDirectory string) error {

	// specification
	specUrlPath, err := s.SpecificationUrlPath()
	if err != nil {
		return err
	}
	if err := s.writeCbfsFile(config, destDirectory, specUrlPath); err != nil {
		return err
	}

	// specification-net
	specNetUrlPath, err := s.SpecificationNetUrlPath()
	if err != nil {
		return err
	}
	if err := s.writeCbfsFile(config, destDirectory, specNetUrlPath); err != nil {
		return err
	}

	return nil
}

// Get a file from cbfs and write it locally
func (s Solver) writeCbfsFile(config Configuration, destDirectory, sourceUrl string) error {

	// get filename, eg, if path is foo/spec.txt, get spec.txt
	_, sourceFilename := filepath.Split(sourceUrl)

	// use cbfs client to open stream

	cbfs, err := cbfsclient.New(config.CbfsUrl)
	if err != nil {
		return err
	}

	// get from cbfs
	logg.LogTo("TRAINING_JOB", "Cbfs get %v", sourceUrl)
	reader, err := cbfs.Get(sourceUrl)
	if err != nil {
		return err
	}

	// write stream to file in work directory
	destPath := filepath.Join(destDirectory, sourceFilename)
	f, err := os.Create(destPath)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	defer w.Flush()
	_, err = io.Copy(w, reader)
	if err != nil {
		return err
	}

	logg.LogTo("TRAINING_JOB", "Wrote to %v", destPath)

	return nil

}

// Download and untar the training and test .tar.gz files associated w/ solver,
// as well as index files.
//
// Returns the label index (each label indexed by its numeric label id), and
// an error or nil
func (s Solver) SaveTrainTestData(config Configuration, destDirectory string) ([]string, error) {

	// find cbfs paths to artificacts
	dataset := NewDataset(config)
	dataset.Id = s.DatasetId
	trainingArtifact := dataset.TrainingArtifactPath()
	testArtifact := dataset.TestingArtifactPath()
	trainingLabelIndex := []string{}
	// TODO: testLabelIndex := []string{}

	artificactPaths := []string{trainingArtifact, testArtifact}
	for _, artificactPath := range artificactPaths {

		// create cbfs client
		cbfs, err := cbfsclient.New(config.CbfsUrl)
		if err != nil {
			return nil, err
		}

		// open stream to artifact in cbfs
		logg.LogTo("TRAINING_JOB", "Cbfs get %v", artificactPath)
		reader, err := cbfs.Get(artificactPath)
		if err != nil {
			return nil, err
		}
		defer reader.Close()

		// Since I'm seeing errors when calling untarGzWithToc:
		//     Err: gzip: invalid header
		// Use a TeeReader to save the raw contents to a file
		_, filename := path.Split(artificactPath)
		destFile := path.Join(destDirectory, filename)
		logg.LogTo("TRAINING_JOB", "Using TeeReader to save copy to %v", destFile)
		f, err := os.Create(destFile)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		teeReader := io.TeeReader(reader, f)

		subdirectory := ""
		destTocFile := ""
		if artificactPath == trainingArtifact {
			subdirectory = TRAINING_DIR
			destTocFile = path.Join(destDirectory, TRAINING_INDEX)
		} else {
			subdirectory = TESTING_DIR
			destTocFile = path.Join(destDirectory, TESTING_INDEX)
		}
		destDirectoryToUse := path.Join(destDirectory, subdirectory)

		toc, err := untarGzWithToc(teeReader, destDirectoryToUse)
		tocWithLabels, labelIndex := addLabelsToToc(toc)
		tocWithSubdir := addParentDirToToc(tocWithLabels, subdirectory)

		if artificactPath == trainingArtifact {
			trainingLabelIndex = labelIndex
		}

		for _, tocEntry := range tocWithSubdir {
			logg.LogTo("TRAINING_JOB", "tocEntry %v", tocEntry)
		}
		if err != nil {
			return nil, err
		}

		writeTocToFile(tocWithSubdir, destTocFile)

	}

	// TODO: make sure trainingLabelIndex == testLabelIndex

	return trainingLabelIndex, nil

}

func writeTocToFile(toc []string, destFile string) error {
	f, err := os.Create(destFile)
	if err != nil {
		logg.LogTo("SOLVER", "calling os.Create failed on %v", destFile)
		return err
	}
	w := bufio.NewWriter(f)
	defer w.Flush()
	for _, tocEntry := range toc {
		tocEntryNewline := fmt.Sprintf("%v\n", tocEntry)
		if _, err := w.WriteString(tocEntryNewline); err != nil {
			return err
		}
	}

	return nil
}

/*
Given a toc:

    Q/Verdana-5-0.png 27
    R/Arial-5-0.png 28

And a parent dir, eg, "training-data", generate a new TOC:

    training-data/Q/Verdana-5-0.png 27
    training-data/R/Arial-5-0.png 28

*/
func addParentDirToToc(tableOfContents []string, dir string) []string {

	tocWithDir := []string{}
	for _, tocEntry := range tableOfContents {
		components := strings.Split(tocEntry, " ")
		file := components[0]
		label := components[1]
		file = path.Join(dir, file)
		line := fmt.Sprintf("%v %v", file, label)
		tocWithDir = append(tocWithDir, line)
	}

	return tocWithDir

}

/*
Given a toc:

    Q/Verdana-5-0.png
    R/Arial-5-0.png

Add a numeric label to each line, eg:

    Q/Verdana-5-0.png 27
    R/Arial-5-0.png 28

Where the label starts at 0 and is incremented for
each new directory found.

Return the new toc with numeric labels, followed by the label index.

*/
func addLabelsToToc(tableOfContents []string) ([]string, []string) {

	currentDirectory := ""
	labelIndex := 0
	tocWithLabels := []string{}
	labels := []string{}

	for _, tocEntry := range tableOfContents {

		dir := path.Dir(tocEntry)
		logg.LogTo("SOLVER", dir)

		if currentDirectory == "" {
			// we're on the first directory
			currentDirectory = dir
		} else {
			// we're not on the first directory, but
			// are we on a new directory?
			if dir == currentDirectory {
				// nope, use curentLabelIndex
			} else {
				// yes, so increment label index
				labelIndex += 1
			}
			currentDirectory = dir
		}

		if !containsString(labels, dir) {
			labels = append(labels, dir)
		}

		tocEntryWithLabel := fmt.Sprintf("%v %v", tocEntry, labelIndex)
		tocWithLabels = append(tocWithLabels, tocEntryWithLabel)

	}

	return tocWithLabels, labels

}

// If spefication url is "cbfs://foo/bar.txt", return "/foo/bar.txt"
func (s Solver) SpecificationUrlPath() (string, error) {

	specUrl := s.SpecificationUrl
	if !strings.HasPrefix(specUrl, CBFS_URI_PREFIX) {
		return "", fmt.Errorf("Expected %v to start with %v", specUrl, CBFS_URI_PREFIX)
	}

	return strings.Replace(specUrl, CBFS_URI_PREFIX, "", 1), nil

}

func (s Solver) SpecificationNetUrlPath() (string, error) {

	specUrl := s.SpecificationNetUrl
	if !strings.HasPrefix(specUrl, CBFS_URI_PREFIX) {
		return "", fmt.Errorf("Expected %v to start with %v", specUrl, CBFS_URI_PREFIX)
	}

	return strings.Replace(specUrl, CBFS_URI_PREFIX, "", 1), nil

}
