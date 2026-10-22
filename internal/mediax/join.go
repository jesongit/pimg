package mediax

import (
 "context"
 "os"
 "path/filepath"
 "strings"
)

// JoinForView is a presentation operation, never an original-file recovery.
func JoinForView(ctx context.Context,paths []string,dest string) error {
 var list strings.Builder
 for _,path:=range paths{list.WriteString("file '");list.WriteString(strings.ReplaceAll(filepath.ToSlash(path),"'","'\\''"));list.WriteString("'\n")}
 file,err:=os.CreateTemp(filepath.Dir(dest),"join-*.txt");if err!=nil{return err};defer os.Remove(file.Name())
 _,err=file.WriteString(list.String());err2:=file.Close();if err!=nil{return err};if err2!=nil{return err2}
 return runVideoCommand(ctx,[]string{"-nostdin","-y","-hide_banner","-loglevel","error","-f","concat","-safe","0","-i",file.Name(),"-map","0:v:0","-map","0:a:0?","-c","copy","-f","matroska",dest})
}
