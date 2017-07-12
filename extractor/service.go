package extractor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	log "github.com/inconshreveable/log15"

	"github.com/bblfsh/sdk/protocol"
	"google.golang.org/grpc"

	"gopkg.in/src-d/go-git.v4"                 // git.Open
	"gopkg.in/src-d/go-git.v4/plumbing"        // Hash, Repository
	"gopkg.in/src-d/go-git.v4/plumbing/object" // object.File
	"gopkg.in/src-d/go-git.v4/storage"

	"gopkg.in/src-d/core-retrieval.v0" // core_retrieval.RootTransactioner
	"gopkg.in/src-d/core.v0"           // core.ModelRepositoryStore
	"gopkg.in/src-d/core.v0/model"     // model.Repository, model.Reference
	"gopkg.in/src-d/enry.v1"           // lang detection
)

type Service struct {
	bblfshClient protocol.ProtocolServiceClient
}

func NewService() *Service {
	//TODO(bzz): parametrize
	bblfshConn, err := grpc.Dial("0.0.0.0:9432", grpc.WithTimeout(time.Second*2), grpc.WithInsecure())
	client := protocol.NewProtocolServiceClient(bblfshConn)
	checkIfError(err)

	return &Service{bblfshClient: client}
}

//proteus:generate
func (s *Service) GetRepositoryData(r *Request) (*RepositoryData, error) {
	// TODO
	return nil, fmt.Errorf("NOT IMPLEMENTED YET")
}

//proteus:generate
func (s *Service) GetRepositoriesData() ([]*RepositoryData, error) {
	n, err := core.ModelRepositoryStore().Count(model.NewRepositoryQuery().FindByStatus(model.Fetched))
	if err != nil {
		log.Error("Could not connect to DB to get the number of 'fetched' repositories", "err", err)
	} else {
		log.Info("Iterating over repositories in DB", "status:fetched", n)
	}

	const master = "refs/heads/master"
	result := make([]*RepositoryData, n)

	reposNum := 0
	totalFiles := 0
	for masterRefInit, repoID := range findAllFetchedReposWithRef(master) {
		log.Info("Processing repository", "id", repoID)
		repo := &RepositoryData{
			RepositoryID: repoID,
			URL:          "", //TODO(bzz): add repo url!
			Files:        make([]File, 100),
		}

		rootedTransactioner := core_retrieval.RootedTransactioner()
		tx, err := rootedTransactioner.Begin(plumbing.Hash(masterRefInit))
		if err != nil {
			log.Error("Failed to begin tx for rooted repo", "id", repoID, "hash", masterRefInit, "err", err)
			continue
		}

		tree, err := gitOpenGetTree(tx.Storer(), repoID, masterRefInit, master)
		if err != nil {
			log.Error("Failed to open&get tree from rooted repo", "id", repoID, "hash", masterRefInit, "err", err)
			_ = tx.Rollback()
			continue
		}

		skpFiles := 0
		sucFiles := 0
		errFiles := 0
		err = tree.Files().ForEach(func(f *object.File) error {
			i := (skpFiles + sucFiles + errFiles) % 1000
			batch := (skpFiles + sucFiles + errFiles) / 1000
			if i == 0 && batch != 0 {
				fmt.Printf("\t%d000 files...\n", batch)
			}

			// discard vendoring with enry
			if enry.IsVendor(f.Name) || enry.IsDotFile(f.Name) ||
				enry.IsDocumentation(f.Name) || enry.IsConfiguration(f.Name) {
				skpFiles++
				return nil
			} //TODO(bzz): filter binaries like .apk and .jar

			// detect language with enry
			fContent, err := f.Contents()
			if err != nil {
				log.Warn("Failed to read", "file", f.Name, "err", err)
				errFiles++
				return nil
			}

			fLang := enry.GetLanguage(f.Name, []byte(fContent))
			if err != nil {
				log.Warn("Failed to detect language", "file", f.Name, "err", err)
				errFiles++
				return nil
			}
			//log.Debug(fmt.Sprintf("\t%-9s blob %s    %s", fLang, f.Hash, f.Name))

			// Babelfish -> UAST (Python, Java)
			if strings.EqualFold(fLang, "java") || strings.EqualFold(fLang, "python") {
				uast, err := parseToUast(s.bblfshClient, f.Name, strings.ToLower(fLang), fContent)
				if err != nil {
					errFiles++
					return nil
				}

				sucFiles++
				file := File{
					Language: fLang,
					Path:     f.Name,
					UAST:     string(*uast), //TODO(bzz): change .proto, make UAST `byte` when using Protobuf (now JSON)
				}
				repo.Files = append(repo.Files, file)
			}
			return nil
		})
		checkIfError(err)
		result = append(result, repo)

		log.Info("Done. All files parsed", "repo", repoID, "success", sucFiles, "fail", errFiles, "skipped", skpFiles)
		reposNum++
		totalFiles = totalFiles + sucFiles + errFiles + skpFiles

		err = tx.Rollback()
		if err != nil {
			log.Error("Failed to rollback tx for rooted repo", "repo", repoID, "err", err)
			continue
		}
	}
	log.Info("Done. All files in all repositories parsed", "repositories", reposNum, "files", totalFiles)
	return result, nil
}

func gitOpenGetTree(txStorer storage.Storer, repoID string, masterRefInit model.SHA1, master string) (*object.Tree, error) {
	rr, err := git.Open(txStorer, nil)
	if err != nil {
		return nil, err
	}

	// look for the reference to orig repo `refs/heads/master/<model.Repository.ID>`
	origHeadOfMaster := plumbing.ReferenceName(fmt.Sprintf("%s/%s", master, repoID))
	branch, err := rr.Reference(origHeadOfMaster, false)
	if err != nil {
		return nil, err
	}

	// retrieve the commit that the reference points to
	commit, err := rr.CommitObject(branch.Hash())
	if err != nil {
		return nil, err
	}

	// iterate over files in that commit
	return commit.Tree()
}

func parseToUast(client protocol.ProtocolServiceClient, fName string, fLang string, fContent string) (*[]byte, error) {
	fName = filepath.Base(fName)
	log.Debug("Parsing file to UAST", "file", fName, "language", fLang)

	//TODO(bzz): take care of non-UTF8 things, before sending them
	//  - either encode in utf8
	//  - or convert to base64() and set encoding param
	req := &protocol.ParseUASTRequest{
		Content:  fContent,
		Language: fLang}
	resp, err := client.ParseUAST(context.TODO(), req)
	if err != nil {
		log.Error("ParseUAST failed on gRPC level", "file", fName, "err", err)
		return nil, err
	} else if resp == nil {
		log.Error("ParseUAST failed on Bblfsh level, response is nil\n")
		return nil, err
	} else if resp.Status != protocol.Ok {
		log.Warn("ParseUAST failed", "file", fName, "satus", resp.Status, "errors num", len(resp.Errors), "errors", resp.Errors)
		return nil, errors.New(resp.Errors[0])
	}

	//data, err := resp.UAST.Marshal()
	//TODO(bzz): change .proto, change back to Protobuf instead of JSON
	data, err := json.Marshal(resp.UAST)
	if err != nil {
		log.Error("Failed to serialize UAST", "file", fName, "err", err)
		return nil, err
	}

	return &data, nil
}

// Collects all Repository metadata in-memory
func findAllFetchedReposWithRef(refText string) map[model.SHA1]string {
	repoStorage := core.ModelRepositoryStore()
	q := model.NewRepositoryQuery().FindByStatus(model.Fetched)
	rs, err := repoStorage.Find(q)
	if err != nil {
		log.Error("Failed to query DB", "err", err)
		return nil
	}

	repos := make(map[model.SHA1]string)
	for rs.Next() { // for each Repository
		repo, err := rs.Get()
		if err != nil {
			log.Error("Failed to get next row from DB", "err", err)
			continue
		}

		var masterRef *model.Reference // find "refs/heads/master".Init
		for _, ref := range repo.References {
			if strings.EqualFold(ref.Name, refText) {
				masterRef = ref
				break
			}
		}
		if masterRef == nil { // skipping repos \wo it
			log.Warn("No reference found ", "repo", repo.ID, "reference", refText)
			continue
		}

		repos[masterRef.Init] = repo.ID.String()
	}
	return repos
}

func checkIfError(err error) {
	if err == nil {
		return
	}

	log.Error("Runtime error", "err", err)
	os.Exit(1)
}
