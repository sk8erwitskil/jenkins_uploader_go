/* Author: Kyle Laplante

  This is an app to upload objects in jenkins to packer

*/

package main

import (
  "bytes"
  "encoding/json"
  "flag"
  "io"
  "log"
  "os"
  "os/exec"
  "os/user"
  "path"
  "path/filepath"
  "regexp"
  "net/http"
  "strings"
)

// a sha1 is 40 characters
var shaMatch = regexp.MustCompile("[0-9a-f]{40}")

// accept .zip, .tgz and .gz files
var acceptableMIMETypes = []string{
  "application/x-compressed",
  "application/x-gzip",
  "application/zip",
}

var AllClusters = []string{
  "atla",
  "smf1",
}

type Package struct {
  Artifact   string
  File       string
  Project    string
  Revision   string
  NeedUpdate map[string]bool
  Updated    map[string]bool  // whether or not the package was successfully updated in packer for each cluster
  Valid      bool  // whether or not the Artifact url has a MIMEType from acceptableMIMETypes
}

type FuturePkg chan *Package

type Futures struct {
  Channels        FuturePkg
  ItemsInChannels int
}

func stringInArray(s string, list []string) bool {
  for _, v := range list {
    if s == v {
      return true
    }
  }
  return false
}

func decodeJsonFile(jsonFile string) FuturePkg {
  future := make(FuturePkg)
  go func() {
    p := &Package{}

    content, err := os.Open(jsonFile)
    if err != nil {
      log.Print("Error:", err)
    }

    jsonParser := json.NewDecoder(content)
    err = jsonParser.Decode(p)
    if err != nil {
      log.Print("Error:", err)
    }

    p.Project = strings.Split(path.Base(jsonFile), ".json")[0]
    future <- p
  }()
  return future
}

func (p *Package) DownloadArtifact(basedir string) FuturePkg {
  future := make(FuturePkg)
  go func() {
    outpath := path.Join(basedir, p.Project)
    outname := path.Join(outpath, path.Base(p.Artifact))
    log.Print("Downloading ", p.Artifact, " to ", outname)

    if _, err := os.Stat(outpath); err == nil {
      os.Remove(outpath)
    }
    err := os.MkdirAll(outpath, 0777)
    if err != nil && !os.IsExist(err) {
      log.Print("Error while creating path ", outpath, " - ", err)
      return
    }

    output, err := os.Create(outname)
    if err != nil {
      log.Print("Error while creating ", outname, " - ", err)
      return
    }
    defer output.Close()

    response, err := http.Get(p.Artifact)
    if err != nil {
      log.Print("Error while downloading ", p.Artifact, " - ", err)
      return
    }
    defer response.Body.Close()

    n, err := io.Copy(output, response.Body)
    if err != nil {
      log.Print("Error while downloading ", p.Artifact, " - ", err)
      return
    }
    log.Print(n, " bytes downloaded for ", p.Project)
    p.File = outname
    future <-p
  }()
  return future
}

func (p *Package) CheckIfValidPackage() FuturePkg {
  future := make(FuturePkg)
  go func() {
    p.Valid = false
    response, err := http.Get(p.Artifact)
    if err == nil && stringInArray(response.Header.Get("content-type"), acceptableMIMETypes) {
      p.Valid = true
    }
    future <- p
  }()
  return future
}

func (p *Package) clusterOkToUpdate(cluster string) (bool, string) {
  cmd := exec.Command("aurora", "package_versions", "--cluster=" + cluster, "jenkins", p.Project)
  var out bytes.Buffer
  cmd.Stdout = &out
  err := cmd.Run()
  if err != nil {
    return true, err.Error()
  }
  shas := shaMatch.FindAllString(out.String(), -1)
  if len(shas) < 1 {
    return true, "No shas found for " + p.Project + " in " + cluster
  }
  latest := shas[len(shas)-1:len(shas)][0]
  log.Print(p.Project, " latest sha in ", cluster, ": ", latest)
  if p.Revision == latest {
    return false, p.Project + " does not need to be updated."
  } else {
    return true, p.Project + " is not up to date in " + cluster
  }
}

func (p *Package) IsUpdateNeeded() FuturePkg {
  future := make(FuturePkg)
  p.NeedUpdate = make(map[string]bool)
  go func() {
    for _, cluster := range AllClusters {
      ok, status := p.clusterOkToUpdate(cluster)
      log.Print("Packer status: " + status)
      p.NeedUpdate[cluster] = ok
    }
    future <- p
  }()
  return future
}

func (p *Package) UpdatePackage(cluster string) FuturePkg {
  future := make(FuturePkg)
  p.Updated = make(map[string]bool)
  go func() {
    current_user, _ := user.Current()
    log.Print(p.Project, ": uploading " + p.File, " to " + current_user.Username + " packer in " + cluster)
    cmd := exec.Command(
        "aurora",
        "package_add_version",
        "--cluster=" + cluster,
        "--metadata=" + p.Revision,
        current_user.Username,
        p.Project,
        p.File,
    )
    var out bytes.Buffer
    cmd.Stdout = &out
    err := cmd.Run()
    if err != nil {
      log.Print(p.Project + ": error: " + err.Error())
      p.Updated[cluster] = false
    } else {
      log.Print(p.Project + ": " + out.String())
      p.Updated[cluster] = true
    }
    future <-p
  }()
  return future
}

func GetAllPackages(path string, futures *Futures) (pkgs []*Package) {
  matches, _ := filepath.Glob(path)

  for _, v := range matches {
    futures.AddFuture(decodeJsonFile(v))
  }

  for ; futures.ItemsInChannels > 0; futures.ItemsInChannels-- {
    pkgs = append(pkgs, <-futures.Channels)
  }
  return
}

func (futures *Futures) BlockUntilComplete(reason string) {
  log.Print(reason)
  for ; futures.ItemsInChannels > 0; futures.ItemsInChannels-- {
    <-futures.Channels  // we dont care whats in the channels... just that they completed
  }
}

func (futures *Futures) AddFuture(future FuturePkg) {
  go func() { for { futures.Channels <- <-future } }()
  futures.ItemsInChannels++
}

func main() {
  var project  string
  var rootPath string

  flag.StringVar(&project, "project", "*", "the project to work on")
  flag.StringVar(&rootPath, "rootpath", "~/workspace/revenue-deploy/config", "the root path to the dir with json files")
  flag.Parse()


  pwd, _ := os.Getwd()
  downloadTmpDir := path.Join(pwd, "downloads_tmp")

  futures := &Futures{Channels: make(FuturePkg), ItemsInChannels: 0}

  pkgs := GetAllPackages(path.Join(rootPath, project + ".json"), futures)
  if len(pkgs) < 1 {
    log.Fatal("No packages found")
  }

  pkgsString := ""
  for _, p := range pkgs {
    pkgsString += p.Project + ", "
  }
  log.Print("Starting projects: ", pkgsString)


  for _, p := range pkgs {
    futures.AddFuture(p.CheckIfValidPackage())
  }
  futures.BlockUntilComplete("Checking validity of packages")

  for _, p := range pkgs {
    if p.Valid {
      log.Print(p.Project, " is valid")
      futures.AddFuture(p.IsUpdateNeeded())
    } else {
      log.Print(p.Project, " is not valid")
    }
  }
  if futures.ItemsInChannels > 0 {
    futures.BlockUntilComplete("Checking which packer clusters need to be updated")
  } else {
    log.Fatal("Unable to process any projects!")
  }

  for _, p := range pkgs {
    for _, cluster := range AllClusters {
      if p.NeedUpdate[cluster] {
        log.Print(p.Project + ": needs update")
        futures.AddFuture(p.DownloadArtifact(downloadTmpDir))
        break  // break if any clusters needs update since we only need the file once for all clusters
      }
    }
  }
  if futures.ItemsInChannels > 0 {
    futures.BlockUntilComplete("Downloading packages")
  } else {
    log.Print("All packages are up to date. Exiting...")
    os.Exit(0)
  }

  for _, p := range pkgs {
    for _, cluster := range AllClusters {
      if p.NeedUpdate[cluster] {
        futures.AddFuture(p.UpdatePackage(cluster))
      }
    }
  }
  // we know there will be items in the channels because we would not have
  // gotten this far if there wasnt because we would have stopped after realizing
  // there was nothing to download.
  futures.BlockUntilComplete("Updating packages")

  for _, p := range pkgs {
    for _, cluster := range AllClusters {
      if p.NeedUpdate[cluster] {
        if p.Updated[cluster] {
          log.Println(p.Project + ": " + cluster + " was updated successfully")
        } else {
          log.Println(p.Project + ": " + cluster + " was NOT updated successfully")
        }
      }
    }
  }
  os.Remove(downloadTmpDir)  // cleanup after yourself!
}
