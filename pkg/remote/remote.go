package remote

import (
  "context"
  "fmt"
  "github.com/aws/aws-sdk-go/aws"
  "io"
  "path"

  "net/http"
  neturl "net/url"
  "os"
  "path/filepath"
  "strconv"
  "strings"

  "github.com/aws/aws-sdk-go/aws/session"
  "github.com/aws/aws-sdk-go/service/s3"

  "github.com/hashicorp/go-getter"
  "github.com/hashicorp/go-getter/helper/url"
  "go.uber.org/multierr"
  "go.uber.org/zap"

  "github.com/helmfile/helmfile/pkg/envvar"
  "github.com/helmfile/helmfile/pkg/filesystem"
)

var disableInsecureFeatures bool

func init() {
  disableInsecureFeatures, _ = strconv.ParseBool(os.Getenv(envvar.DisableInsecureFeatures))
}

func CacheDir() string {
  if h := os.Getenv(envvar.CacheHome); h != "" {
    return h
  }

  dir, err := os.UserCacheDir()
  if err != nil {
    // fall back to relative path with hidden directory
    return ".helmfile"
  }
  return filepath.Join(dir, "helmfile")
}

type Remote struct {
  Logger *zap.SugaredLogger

  // Home is the directory in which remote downloads files. If empty, user cache directory is used
  Home string

  // Getter is the underlying implementation of getter used for fetching remote files
  Getter Getter

  S3Getter Getter

  HttpGetter Getter

  // Filesystem abstraction
  // Inject any implementation of your choice, like an im-memory impl for testing, os.ReadFile for the real-world use.
  fs *filesystem.FileSystem
}

// Locate takes an URL to a remote file or a path to a local file.
// If the argument was an URL, it fetches the remote directory contained within the URL,
// and returns the path to the file in the fetched directory
func (r *Remote) Locate(urlOrPath string, cacheDirOpt ...string) (string, error) {
  if r.fs.FileExistsAt(urlOrPath) || r.fs.DirectoryExistsAt(urlOrPath) {
    return urlOrPath, nil
  }
  fetched, err := r.Fetch(urlOrPath, cacheDirOpt...)
  if err != nil {
    if _, ok := err.(InvalidURLError); ok {
      return urlOrPath, nil
    }
    return "", err
  }
  return fetched, nil
}

type InvalidURLError struct {
  err string
}

func (e InvalidURLError) Error() string {
  return e.err
}

type Source struct {
  Getter, Scheme, User, Host, Dir, File, RawQuery string
}

func IsRemote(goGetterSrc string) bool {
  if _, err := Parse(goGetterSrc); err != nil {
    return false
  }
  return true
}

func Parse(goGetterSrc string) (*Source, error) {
  items := strings.Split(goGetterSrc, "::")
  var getter string
  if len(items) == 2 {
    getter = items[0]
    goGetterSrc = items[1]
  } else {
    items = strings.Split(goGetterSrc, "://")

    if len(items) == 2 {
      return ParseNormal(goGetterSrc)
    }
  }

  u, err := url.Parse(goGetterSrc)
  if err != nil {
    return nil, InvalidURLError{err: fmt.Sprintf("parse url: %v", err)}
  }

  if u.Scheme == "" {
    return nil, InvalidURLError{err: fmt.Sprintf("parse url: missing scheme - probably this is a local file path? %s", goGetterSrc)}
  }

  pathComponents := strings.Split(u.Path, "@")
  if len(pathComponents) != 2 {
    dir := filepath.Dir(u.Path)
    if dir == "/" {
      dir = ""
    }
    pathComponents = []string{dir, filepath.Base(u.Path)}
  }

  return &Source{
    Getter:   getter,
    User:     u.User.String(),
    Scheme:   u.Scheme,
    Host:     u.Host,
    Dir:      pathComponents[0],
    File:     pathComponents[1],
    RawQuery: u.RawQuery,
  }, nil
}

func ParseNormal(path string) (*Source, error) {
  _, err := ParseNormalProtocol(path)
  if err != nil {
    return nil, err
  }

  u, err := url.Parse(path)
  dir := filepath.Dir(u.Path)
  if dir == "/" {
    dir = ""
  }

  return &Source{
    Getter:   "normal",
    User:     u.User.String(),
    Scheme:   u.Scheme,
    Host:     u.Host,
    Dir:      dir,
    File:     filepath.Base(u.Path),
    RawQuery: u.RawQuery,
  }, err
}

func ParseNormalProtocol(path string) (string, error) {
  parts := strings.Split(path, "://")

  if len(parts) == 0 {
    return "", fmt.Errorf("failed to parse URL %s", path)
  }
  protocol := strings.ToLower(parts[0])

  protocols := []string{"s3", "http", "https"}
  for _, option := range protocols {
    if option == protocol {
      return protocol, nil
    }
  }
  return "", fmt.Errorf("failed to parse URL %s", path)
}

func (r *Remote) Fetch(path string, cacheDirOpt ...string) (string, error) {
  u, err := Parse(path)
  if err != nil {
    return "", err
  }

  srcDir := fmt.Sprintf("%s://%s%s", u.Scheme, u.Host, u.Dir)
  file := u.File

  r.Logger.Debugf("remote> getter: %s", u.Getter)
  r.Logger.Debugf("remote> scheme: %s", u.Scheme)
  r.Logger.Debugf("remote> user: %s", u.User)
  r.Logger.Debugf("remote> host: %s", u.Host)
  r.Logger.Debugf("remote> dir: %s", u.Dir)
  r.Logger.Debugf("remote> file: %s", u.File)

  // This should be shared across variant commands, so that they can share cache for the shared imports
  cacheBaseDir := ""
  if len(cacheDirOpt) == 1 {
    cacheBaseDir = cacheDirOpt[0]
  } else if len(cacheDirOpt) > 0 {
    return "", fmt.Errorf("[bug] cacheDirOpt's length: want 0 or 1, got %d", len(cacheDirOpt))
  }

  query := u.RawQuery

  var cacheKey string
  replacer := strings.NewReplacer(":", "", "//", "_", "/", "_", ".", "_")
  dirKey := replacer.Replace(srcDir)
  if len(query) > 0 {
    q, _ := neturl.ParseQuery(query)
    if q.Has("sshkey") {
      q.Set("sshkey", "redacted")
    }
    paramsKey := strings.ReplaceAll(q.Encode(), "&", "_")
    cacheKey = fmt.Sprintf("%s.%s", dirKey, paramsKey)
  } else {
    cacheKey = dirKey
  }

  cached := false

  // e.g. https_github_com_cloudposse_helmfiles_git.ref=0.xx.0
  getterDst := filepath.Join(cacheBaseDir, cacheKey)

  // e.g. os.CacheDir()/helmfile/https_github_com_cloudposse_helmfiles_git.ref=0.xx.0
  cacheDirPath := filepath.Join(r.Home, getterDst)

  r.Logger.Debugf("remote> home: %s", r.Home)
  r.Logger.Debugf("remote> getter dest: %s", getterDst)
  r.Logger.Debugf("remote> cached dir: %s", cacheDirPath)

  {
    if r.fs.FileExistsAt(cacheDirPath) {
      return "", fmt.Errorf("%s is not directory. please remove it so that variant could use it for dependency caching", getterDst)
    }

    cachedFilePath := filepath.Join(cacheDirPath, file)
    if u.Getter == "normal" && r.fs.FileExistsAt(cachedFilePath) {
      cached = true
    } else if r.fs.DirectoryExistsAt(cacheDirPath) {
      cached = true
    }
  }

  if !cached {
    var getterSrc string
    if u.User != "" {
      getterSrc = fmt.Sprintf("%s://%s@%s%s", u.Scheme, u.User, u.Host, u.Dir)
    } else {
      getterSrc = fmt.Sprintf("%s://%s%s", u.Scheme, u.Host, u.Dir)
    }

    if len(query) > 0 {
      getterSrc = strings.Join([]string{getterSrc, query}, "?")
    }

    r.Logger.Debugf("remote> downloading %s to %s", getterSrc, getterDst)

    if u.Getter == "normal" && u.Scheme == "s3" {

      err := r.S3Getter.Get(r.Home, path, cacheDirPath)
      if err != nil {
        return "", multierr.Append(err, err)
      }

    } else if u.Getter == "normal" && (u.Scheme == "https" || u.Scheme == "http") {
      err := r.HttpGetter.Get(r.Home, path, cacheDirPath)
      if err != nil {
        return "", multierr.Append(err, err)
      }
    } else {
      if u.Getter != "" {
        getterSrc = u.Getter + "::" + getterSrc
      }

      if err := r.Getter.Get(r.Home, getterSrc, cacheDirPath); err != nil {
        rmerr := os.RemoveAll(cacheDirPath)
        if rmerr != nil {
          return "", multierr.Append(err, rmerr)
        }
        return "", err
      }
    }

  }

  return filepath.Join(cacheDirPath, file), nil
}

type Getter interface {
  Get(wd, src, dst string) error
}

type GoGetter struct {
  Logger *zap.SugaredLogger
}

type S3Getter struct {
  Logger *zap.SugaredLogger
}

type HttpGetter struct {
  Logger *zap.SugaredLogger
}

func (g *GoGetter) Get(wd, src, dst string) error {
  ctx := context.Background()

  get := &getter.Client{
    Ctx:     ctx,
    Src:     src,
    Dst:     dst,
    Pwd:     wd,
    Mode:    getter.ClientModeDir,
    Options: []getter.ClientOption{},
  }

  g.Logger.Debugf("client: %+v", *get)

  if err := get.Get(); err != nil {
    return fmt.Errorf("get: %v", err)
  }

  return nil
}

func (g *S3Getter) Get(wd, src, dst string) error {

  u, err := url.Parse(src)
  if err != nil {
    return err
  }
  file := path.Base(u.Path)
  targetFilePath := filepath.Join(dst, file)

  region, err := S3FileExists(src)
  if err != nil {
    return err
  }

  bucket, key, err := ParseS3Url(src)
  if err != nil {
    return err
  }

  err = os.MkdirAll(dst, os.FileMode(0700))
  if err != nil {
    return err
  }

  // Create a new AWS session using the default AWS configuration
  sess := session.Must(session.NewSessionWithOptions(session.Options{
    SharedConfigState: session.SharedConfigEnable,
    Config: aws.Config{
      Region: aws.String(region),
    },
  }))
  if err != nil {
    return err
  }

  // Create an S3 client using the session
  s3Client := s3.New(sess)

  getObjectInput := &s3.GetObjectInput{
    Bucket: &bucket,
    Key:    &key,
  }
  resp, err := s3Client.GetObject(getObjectInput)
  defer func(Body io.ReadCloser) {
    err := Body.Close()
    if err != nil {
      g.Logger.Errorf("Error closing connection to remote data source \n%v", err)
    }
  }(resp.Body)

  if err != nil {
    return err
  }

  localFile, err := os.Create(targetFilePath)
  if err != nil {
    return err
  }
  defer func(localFile *os.File) {
    err := localFile.Close()
    if err != nil {
      g.Logger.Errorf("Error writing file \n%v", err)
    }
  }(localFile)

  _, err = localFile.ReadFrom(resp.Body)
  if err != nil {
    return err
  }

  return nil
}

func (g *HttpGetter) Get(wd, src, dst string) error {

  u, err := url.Parse(src)
  if err != nil {
    return err
  }
  file := path.Base(u.Path)
  targetFilePath := filepath.Join(dst, file)

  err = HttpFileExists(src)
  if err != nil {
    return err
  }

  err = os.MkdirAll(dst, os.FileMode(0700))
  if err != nil {
    return err
  }

  resp, err := http.Get(src)
  defer func(Body io.ReadCloser) {
    err := Body.Close()
    if err != nil {
      fmt.Printf("Error %v", err)
      g.Logger.Errorf("Error closing connection to remote data source\n%v", err)
    }
  }(resp.Body)

  if err != nil {
    fmt.Printf("Error %v", err)
    return err
  }

  localFile, err := os.Create(targetFilePath)
  if err != nil {
    return err
  }
  defer func(localFile *os.File) {
    err := localFile.Close()
    if err != nil {
      g.Logger.Errorf("Error writing file \n%v", err)
    }
  }(localFile)

  _, err = localFile.ReadFrom(resp.Body)
  if err != nil {
    return err
  }

  return nil
}

func S3FileExists(path string) (string, error) {

  bucket, key, err := ParseS3Url(path)
  if err != nil {
    return "", err
  }

  // Region
  sess := session.Must(session.NewSessionWithOptions(session.Options{
    SharedConfigState: session.SharedConfigEnable,
  }))
  if err != nil {
    return "", fmt.Errorf("failed to authentication with aws %w", err)
  }

  s3Client := s3.New(sess)
  getBucketLocationInput := &s3.GetBucketLocationInput{
    Bucket: aws.String(bucket),
  }
  resp, err := s3Client.GetBucketLocation(getBucketLocationInput)
  if err != nil {
    return "", fmt.Errorf("Error: Failed to retrieve bucket location: %v\n", err)
  }

  // File existence
  s3Client = s3.New(sess)
  headObjectInput := &s3.HeadObjectInput{
    Bucket: &bucket,
    Key:    &key,
  }

  _, err = s3Client.HeadObject(headObjectInput)
  return *resp.LocationConstraint, err
}

func HttpFileExists(path string) error {
  _, err := http.Head(path)
  return err
}

func ParseS3Url(s3URL string) (string, string, error) {
  parsedURL, err := url.Parse(s3URL)
  if err != nil {
    return "", "", fmt.Errorf("failed to parse S3 URL: %w", err)
  }

  if parsedURL.Scheme != "s3" {
    return "", "", fmt.Errorf("invalid URL scheme (expected 's3')")
  }

  bucket := parsedURL.Host
  key := strings.TrimPrefix(parsedURL.Path, "/")

  return bucket, key, nil
}

func NewRemote(logger *zap.SugaredLogger, homeDir string, fs *filesystem.FileSystem) *Remote {
  if disableInsecureFeatures {
    panic("Remote sources are disabled due to 'DISABLE_INSECURE_FEATURES'")
  }
  remote := &Remote{
    Logger:     logger,
    Home:       homeDir,
    Getter:     &GoGetter{Logger: logger},
    S3Getter:   &S3Getter{Logger: logger},
    HttpGetter: &HttpGetter{Logger: logger},
    fs:         fs,
  }

  if remote.Home == "" {
    // Use for remote charts
    remote.Home = CacheDir()
  }

  return remote
}
